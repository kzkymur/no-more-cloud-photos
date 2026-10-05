// Project-owned source. See ../LICENSE.md.
#include "nmcp_animation/codec_pipeline.h"

#include <aom/aom_codec.h>
#include <algorithm>
#include <array>
#include <cerrno>
#include <cstring>
#include <gif_lib.h>
#include <lcms2.h>
#include <libheif/heif.h>
#include <limits>
#include <memory>
#include <sstream>
#include <sys/stat.h>
#include <unistd.h>
#include <webp/decode.h>
#include <webp/demux.h>
#include <webp/encode.h>
#include <webp/mux.h>
#include <webp/mux_types.h>

namespace nmcp_animation {
namespace {

constexpr std::string_view kBuildManifest =
    "giflib=5.2.2;libwebp=1.6.0;simd=on;threads=on;near-lossless=on";

using ProfilePtr = std::unique_ptr<void, decltype(&cmsCloseProfile)>;
using TransformPtr = std::unique_ptr<void, decltype(&cmsDeleteTransform)>;

struct GifCloser {
  void operator()(GifFileType *gif) const {
    int error = 0;
    if (gif) (void)DGifCloseFile(gif, &error);
  }
};

struct WebPDemuxCloser { void operator()(WebPDemuxer *value) const { WebPDemuxDelete(value); } };
struct WebPAnimCloser { void operator()(WebPAnimDecoder *value) const { WebPAnimDecoderDelete(value); } };
struct WebPMuxCloser { void operator()(WebPMux *value) const { WebPMuxDelete(value); } };

struct WebPMemoryWriter {
  std::vector<std::uint8_t> bytes;
  std::uint64_t maximum{};
  bool exceeded{};
};

int write_webp(const std::uint8_t *data, std::size_t size, const WebPPicture *picture) {
  auto &writer = *static_cast<WebPMemoryWriter *>(picture->custom_ptr);
  if (size > writer.maximum - writer.bytes.size()) {
    writer.exceeded = true;
    return 0;
  }
  writer.bytes.insert(writer.bytes.end(), data, data + size);
  return 1;
}

std::string json_versions(const std::map<std::string, std::string> &versions) {
  std::ostringstream output;
  output << '{';
  bool first = true;
  for (const auto &[name, version] : versions) {
    if (!first) output << ',';
    first = false;
    output << json_string(name) << ':' << json_string(version);
  }
  output << '}';
  return output.str();
}

std::string webp_version(unsigned value) {
  return std::to_string((value >> 16U) & 0xffU) + "." + std::to_string((value >> 8U) & 0xffU) + "." +
         std::to_string(value & 0xffU);
}

std::string lcms_version() {
  const unsigned value = cmsGetEncodedCMMversion();
  return std::to_string(value / 1000U) + "." + std::to_string((value / 10U) % 100U);
}

void validate_srgb(std::span<const std::byte> bytes) {
  if (bytes.empty() || bytes.size() > 16U << 20U) throw Failure("capability_failed");
  ProfilePtr profile(cmsOpenProfileFromMem(bytes.data(), static_cast<cmsUInt32Number>(bytes.size())), cmsCloseProfile);
  if (!profile || cmsGetColorSpace(profile.get()) != cmsSigRgbData || cmsGetPCS(profile.get()) != cmsSigXYZData) {
    throw Failure("capability_failed");
  }
}

bool alpha_present(const Image &image) {
  for (std::size_t offset = 3; offset < image.rgba.size(); offset += 4) if (image.rgba[offset] != 255) return true;
  return false;
}

void validate_embedded_profile(std::span<const std::byte> bytes) {
  if (bytes.empty()) return;
  ProfilePtr profile(cmsOpenProfileFromMem(bytes.data(), static_cast<cmsUInt32Number>(bytes.size())), cmsCloseProfile);
  if (!profile || cmsGetColorSpace(profile.get()) != cmsSigRgbData) throw Failure("unsupported_input");
}

void normalize_color(Animation &animation, std::span<const std::byte> destination) {
  if (animation.embedded_icc.empty()) return;
  ProfilePtr input(cmsOpenProfileFromMem(animation.embedded_icc.data(),
                                         static_cast<cmsUInt32Number>(animation.embedded_icc.size())), cmsCloseProfile);
  ProfilePtr output(cmsOpenProfileFromMem(destination.data(), static_cast<cmsUInt32Number>(destination.size())), cmsCloseProfile);
  if (!input || !output || cmsGetColorSpace(input.get()) != cmsSigRgbData || cmsGetColorSpace(output.get()) != cmsSigRgbData) {
    throw Failure("unsupported_input");
  }
  TransformPtr transform(cmsCreateTransform(input.get(), TYPE_RGBA_8, output.get(), TYPE_RGBA_8,
                                             INTENT_RELATIVE_COLORIMETRIC,
                                             cmsFLAGS_BLACKPOINTCOMPENSATION | cmsFLAGS_COPY_ALPHA), cmsDeleteTransform);
  if (!transform) throw Failure("processing_failed");
  for (auto &frame : animation.frames) {
    const auto pixels = static_cast<cmsUInt32Number>(static_cast<std::uint64_t>(frame.width) * frame.height);
    cmsDoTransform(transform.get(), frame.rgba.data(), frame.rgba.data(), pixels);
  }
}

void clear_rect(Image &canvas, int x, int y, int width, int height) {
  for (int row = y; row < y + height; ++row) {
    auto begin = canvas.rgba.begin() + (static_cast<std::size_t>(row) * canvas.width + x) * 4U;
    std::fill(begin, begin + static_cast<std::ptrdiff_t>(width) * 4, 0);
  }
}

Animation decode_gif(const std::string &path, const Limits &limits, bool pixels_wanted) {
  int gif_error = 0;
  std::unique_ptr<GifFileType, GifCloser> gif(DGifOpenFileName(path.c_str(), &gif_error));
  if (!gif) throw Failure("decode_failed");
  if (gif->SWidth <= 0 || gif->SHeight <= 0 || gif->SWidth > limits.dimension || gif->SHeight > limits.dimension ||
      static_cast<std::uint64_t>(gif->SWidth) * gif->SHeight > limits.canvas_pixels) throw Failure("resource_limit");

  Image canvas{.width = gif->SWidth, .height = gif->SHeight,
               .rgba = std::vector<std::uint8_t>(static_cast<std::size_t>(gif->SWidth) * gif->SHeight * 4U)};
  std::vector<FrameInfo> infos;
  std::vector<Image> frames;
  GraphicsControlBlock control{.DisposalMode = DISPOSAL_UNSPECIFIED, .UserInputFlag = false,
                               .DelayTime = 0, .TransparentColor = NO_TRANSPARENT_COLOR};
  bool loop_present = false;
  bool control_present = false;
  unsigned repetitions = 0;
  std::vector<std::byte> gif_icc;
  int previous_disposal = DISPOSAL_UNSPECIFIED;
  int previous_x = 0, previous_y = 0, previous_width = 0, previous_height = 0;
  Image restore;
  GifRecordType record = UNDEFINED_RECORD_TYPE;
  while (record != TERMINATE_RECORD_TYPE) {
    if (DGifGetRecordType(gif.get(), &record) == GIF_ERROR) throw Failure("decode_failed");
    if (record == EXTENSION_RECORD_TYPE) {
      int code = 0;
      GifByteType *block = nullptr;
      if (DGifGetExtension(gif.get(), &code, &block) == GIF_ERROR) throw Failure("decode_failed");
      std::string application;
      bool first = true;
      if (code == PLAINTEXT_EXT_FUNC_CODE) throw Failure("unsupported_input");
      while (block != nullptr) {
        const int size = block[0];
        if (code == GRAPHICS_EXT_FUNC_CODE) {
          if (!first || size != 4 || control_present) throw Failure("decode_failed");
          control_present = true;
          control.DisposalMode = (block[1] >> 2) & 7;
          control.UserInputFlag = (block[1] & 2) != 0;
          if (control.UserInputFlag) throw Failure("unsupported_input");
          control.DelayTime = block[2] | (block[3] << 8);
          control.TransparentColor = (block[1] & 1) != 0 ? block[4] : NO_TRANSPARENT_COLOR;
        } else if (code == APPLICATION_EXT_FUNC_CODE && first && size == 11) {
          application.assign(reinterpret_cast<char *>(block + 1), 11);
        } else if (code == APPLICATION_EXT_FUNC_CODE &&
                   (application == "NETSCAPE2.0" || application == "ANIMEXTS1.0") &&
                   size == 3 && block[1] == 1) {
          if (loop_present) throw Failure("unsupported_input");
          loop_present = true;
          repetitions = static_cast<unsigned>(block[2]) | (static_cast<unsigned>(block[3]) << 8U);
        } else if (code == APPLICATION_EXT_FUNC_CODE && application == "ICCRGBG1012") {
          if (static_cast<std::size_t>(size) > (16U << 20U) - gif_icc.size()) throw Failure("resource_limit");
          const auto *begin = reinterpret_cast<const std::byte *>(block + 1);
          gif_icc.insert(gif_icc.end(), begin, begin + size);
        }
        first = false;
        if (DGifGetExtensionNext(gif.get(), &block) == GIF_ERROR) throw Failure("decode_failed");
      }
    } else if (record == IMAGE_DESC_RECORD_TYPE) {
      if (infos.size() >= static_cast<std::size_t>(limits.frames)) throw Failure("resource_limit");
      if (DGifGetImageDesc(gif.get()) == GIF_ERROR) throw Failure("decode_failed");
      const GifImageDesc &desc = gif->Image;
      if (control.DisposalMode < DISPOSAL_UNSPECIFIED || control.DisposalMode > DISPOSE_PREVIOUS) {
        throw Failure("decode_failed");
      }
      FrameInfo info{.x = desc.Left, .y = desc.Top, .width = desc.Width, .height = desc.Height,
                     .duration_ms = control.DelayTime * 10, .has_alpha = control.TransparentColor != NO_TRANSPARENT_COLOR};
      // Account the source rectangle before asking giflib to decompress its raster.
      std::vector<FrameInfo> prospective = infos;
      prospective.push_back(info);
      (void)make_inspection("animation", gif->SWidth, gif->SHeight, prospective,
                            gif_total_plays(loop_present, repetitions), limits);
      if (previous_disposal == DISPOSE_BACKGROUND) clear_rect(canvas, previous_x, previous_y, previous_width, previous_height);
      else if (previous_disposal == DISPOSE_PREVIOUS && !restore.rgba.empty()) canvas = restore;
      Image before = canvas;
      ColorMapObject *colors = desc.ColorMap != nullptr ? desc.ColorMap : gif->SColorMap;
      if (!colors) throw Failure("decode_failed");
      if (control.TransparentColor != NO_TRANSPARENT_COLOR && control.TransparentColor >= colors->ColorCount) {
        throw Failure("decode_failed");
      }
      std::vector<GifPixelType> indices(static_cast<std::size_t>(desc.Width) * desc.Height);
      static constexpr std::array<int, 4> starts = {0, 4, 2, 1};
      static constexpr std::array<int, 4> steps = {8, 8, 4, 2};
      if (desc.Interlace) {
        for (std::size_t pass = 0; pass < starts.size(); ++pass) {
          for (int row = starts[pass]; row < desc.Height; row += steps[pass]) {
            if (DGifGetLine(gif.get(), indices.data() + static_cast<std::size_t>(row) * desc.Width, desc.Width) == GIF_ERROR) throw Failure("decode_failed");
          }
        }
      } else if (DGifGetLine(gif.get(), indices.data(), desc.Width * desc.Height) == GIF_ERROR) {
        throw Failure("decode_failed");
      }
      for (int y = 0; y < desc.Height; ++y) for (int x = 0; x < desc.Width; ++x) {
        const int index = indices[static_cast<std::size_t>(y) * desc.Width + x];
        if (index == control.TransparentColor) continue;
        if (index < 0 || index >= colors->ColorCount) throw Failure("decode_failed");
        const GifColorType color = colors->Colors[index];
        const std::size_t offset = (static_cast<std::size_t>(desc.Top + y) * canvas.width + desc.Left + x) * 4U;
        canvas.rgba[offset] = color.Red; canvas.rgba[offset + 1] = color.Green;
        canvas.rgba[offset + 2] = color.Blue; canvas.rgba[offset + 3] = 255;
      }
      info.has_alpha = alpha_present(canvas);
      infos.push_back(info);
      if (pixels_wanted) frames.push_back(canvas);
      restore = control.DisposalMode == DISPOSE_PREVIOUS ? std::move(before) : Image{};
      previous_disposal = control.DisposalMode;
      previous_x = desc.Left; previous_y = desc.Top; previous_width = desc.Width; previous_height = desc.Height;
      control = {.DisposalMode = DISPOSAL_UNSPECIFIED, .UserInputFlag = false,
                 .DelayTime = 0, .TransparentColor = NO_TRANSPARENT_COLOR};
      control_present = false;
    }
  }
  if (infos.empty()) throw Failure("decode_failed");
  Animation result;
  result.inspection = make_inspection("animation", gif->SWidth, gif->SHeight, infos,
                                      gif_total_plays(loop_present, repetitions), limits);
  result.frames = std::move(frames);
  result.embedded_icc = std::move(gif_icc);
  validate_embedded_profile(result.embedded_icc);
  result.decoder = "giflib-gif";
  return result;
}

Animation decode_webp(const std::string &path, const Limits &limits, bool pixels_wanted) {
  const auto bytes = read_regular_file(path, static_cast<std::size_t>(kAbsoluteMaxPixels));
  WebPData data{reinterpret_cast<const std::uint8_t *>(bytes.data()), bytes.size()};
  std::unique_ptr<WebPDemuxer, WebPDemuxCloser> demux(WebPDemux(&data));
  if (!demux) throw Failure("decode_failed");
  const int width = static_cast<int>(WebPDemuxGetI(demux.get(), WEBP_FF_CANVAS_WIDTH));
  const int height = static_cast<int>(WebPDemuxGetI(demux.get(), WEBP_FF_CANVAS_HEIGHT));
  const int count = static_cast<int>(WebPDemuxGetI(demux.get(), WEBP_FF_FRAME_COUNT));
  if (count <= 0 || count > limits.frames) throw Failure("resource_limit");
  std::vector<FrameInfo> infos;
  WebPIterator iterator{};
  if (!WebPDemuxGetFrame(demux.get(), 1, &iterator)) throw Failure("decode_failed");
  do {
    infos.push_back({.x = iterator.x_offset, .y = iterator.y_offset, .width = iterator.width,
                     .height = iterator.height, .duration_ms = iterator.duration,
                     .has_alpha = iterator.has_alpha != 0});
    // Enforce every cumulative ceiling before the next frame is decoded.
    (void)make_inspection(count == 1 ? "static" : "animation", width, height, infos,
                          count == 1 ? 1 : static_cast<int>(WebPDemuxGetI(demux.get(), WEBP_FF_LOOP_COUNT)), limits);
  } while (WebPDemuxNextFrame(&iterator));
  WebPDemuxReleaseIterator(&iterator);
  if (infos.size() != static_cast<std::size_t>(count)) throw Failure("decode_failed");

  Animation result;
  result.inspection = make_inspection(count == 1 ? "static" : "animation", width, height, infos,
                                      count == 1 ? 1 : static_cast<int>(WebPDemuxGetI(demux.get(), WEBP_FF_LOOP_COUNT)), limits);
  result.decoder = "libwebp-animation";
  WebPChunkIterator chunk{};
  if (WebPDemuxGetChunk(demux.get(), "ICCP", 1, &chunk)) {
    if (chunk.chunk.size > 16U << 20U) { WebPDemuxReleaseChunkIterator(&chunk); throw Failure("resource_limit"); }
    const auto *begin = reinterpret_cast<const std::byte *>(chunk.chunk.bytes);
    result.embedded_icc.assign(begin, begin + chunk.chunk.size);
    WebPDemuxReleaseChunkIterator(&chunk);
  }
  validate_embedded_profile(result.embedded_icc);
  WebPAnimDecoderOptions options{};
  if (!WebPAnimDecoderOptionsInit(&options)) throw Failure("capability_failed");
  options.color_mode = MODE_RGBA;
  options.use_threads = 0;
  std::unique_ptr<WebPAnimDecoder, WebPAnimCloser> decoder(WebPAnimDecoderNew(&data, &options));
  if (!decoder) throw Failure("decode_failed");
  std::uint8_t *rgba = nullptr;
  int timestamp = 0;
  std::size_t decoded_count = 0;
  int expected_timestamp = 0;
  bool composited_alpha = false;
  while (WebPAnimDecoderHasMoreFrames(decoder.get())) {
    if (!WebPAnimDecoderGetNext(decoder.get(), &rgba, &timestamp) || rgba == nullptr) throw Failure("decode_failed");
    if (decoded_count >= infos.size() || infos[decoded_count].duration_ms > std::numeric_limits<int>::max() - expected_timestamp) {
      throw Failure("decode_failed");
    }
    expected_timestamp += infos[decoded_count].duration_ms;
    if (timestamp != expected_timestamp) throw Failure("decode_failed");
    ++decoded_count;
    if (!composited_alpha) {
      const std::size_t bytes_count = static_cast<std::size_t>(width) * height * 4U;
      for (std::size_t offset = 3; offset < bytes_count; offset += 4) {
        if (rgba[offset] != 255) { composited_alpha = true; break; }
      }
    }
    if (pixels_wanted) {
      Image frame{.width = width, .height = height,
                  .rgba = std::vector<std::uint8_t>(rgba, rgba + static_cast<std::size_t>(width) * height * 4U)};
      result.frames.push_back(std::move(frame));
    }
  }
  if (decoded_count != infos.size()) throw Failure("decode_failed");
  result.inspection.has_alpha = composited_alpha;
  return result;
}

Animation decode(const std::string &path, const std::string &mime, const Limits &limits, bool pixels) {
  if (mime == "image/gif") return decode_gif(path, limits, pixels);
  if (mime == "image/webp") return decode_webp(path, limits, pixels);
  throw Failure("unsupported_input");
}

std::vector<std::uint8_t> encode_webp(const Animation &animation, int quality,
                                      int width, int height, std::span<const std::byte> icc,
                                      std::uint64_t maximum) {
  std::unique_ptr<WebPMux, WebPMuxCloser> mux(WebPMuxNew());
  if (!mux) throw Failure("encode_failed");
  WebPMuxAnimParams params{.bgcolor = 0, .loop_count = animation.inspection.total_plays};
  if (WebPMuxSetAnimationParams(mux.get(), &params) != WEBP_MUX_OK) throw Failure("encode_failed");
  WebPData profile{reinterpret_cast<const std::uint8_t *>(icc.data()), icc.size()};
  if (WebPMuxSetChunk(mux.get(), "ICCP", &profile, 1) != WEBP_MUX_OK) throw Failure("encode_failed");
  WebPConfig config{};
  if (!WebPConfigInit(&config)) throw Failure("capability_failed");
  config.quality = static_cast<float>(quality);
  config.method = 6;
  config.thread_level = 0;
  if (!WebPValidateConfig(&config)) throw Failure("encode_failed");
  for (std::size_t index = 0; index < animation.frames.size(); ++index) {
    const Image resized = resize_premultiplied(animation.frames[index], width, height);
    WebPPicture picture{};
    if (!WebPPictureInit(&picture)) throw Failure("capability_failed");
    picture.width = width; picture.height = height; picture.use_argb = 1;
    WebPMemoryWriter writer{.bytes = {}, .maximum = maximum, .exceeded = false};
    picture.writer = write_webp;
    picture.custom_ptr = &writer;
    if (!WebPPictureImportRGBA(&picture, resized.rgba.data(), width * 4) || !WebPEncode(&config, &picture)) {
      WebPPictureFree(&picture);
      throw Failure(writer.exceeded ? "output_too_large" : "encode_failed");
    }
    WebPPictureFree(&picture);
    WebPMuxFrameInfo frame{};
    frame.bitstream = {writer.bytes.data(), writer.bytes.size()};
    frame.duration = animation.inspection.frame_durations_ms[index];
    frame.id = WEBP_CHUNK_ANMF;
    frame.dispose_method = WEBP_MUX_DISPOSE_NONE;
    frame.blend_method = WEBP_MUX_NO_BLEND;
    if (WebPMuxPushFrame(mux.get(), &frame, 1) != WEBP_MUX_OK) throw Failure("encode_failed");
  }
  WebPData output{};
  WebPDataInit(&output);
  const WebPMuxError assemble_error = WebPMuxAssemble(mux.get(), &output);
  const bool exceeded = output.size > maximum;
  if (assemble_error != WEBP_MUX_OK || output.size == 0 || exceeded) {
    WebPDataClear(&output);
    throw Failure(exceeded ? "output_too_large" : "encode_failed");
  }
  std::vector<std::uint8_t> result(output.bytes, output.bytes + output.size);
  WebPDataClear(&output);
  WebPData check_data{result.data(), result.size()};
  std::unique_ptr<WebPDemuxer, WebPDemuxCloser> check(WebPDemux(&check_data));
  if (!check || WebPDemuxGetI(check.get(), WEBP_FF_CANVAS_WIDTH) != static_cast<std::uint32_t>(width) ||
      WebPDemuxGetI(check.get(), WEBP_FF_CANVAS_HEIGHT) != static_cast<std::uint32_t>(height) ||
      WebPDemuxGetI(check.get(), WEBP_FF_FRAME_COUNT) != static_cast<std::uint32_t>(animation.inspection.frame_durations_ms.size()) ||
      WebPDemuxGetI(check.get(), WEBP_FF_LOOP_COUNT) != static_cast<std::uint32_t>(animation.inspection.total_plays)) {
    throw Failure("encode_failed");
  }
  WebPIterator frame{};
  if (!WebPDemuxGetFrame(check.get(), 1, &frame)) throw Failure("encode_failed");
  std::size_t index = 0;
  do {
    if (index >= animation.inspection.frame_durations_ms.size() ||
        frame.duration != animation.inspection.frame_durations_ms[index]) {
      WebPDemuxReleaseIterator(&frame);
      throw Failure("encode_failed");
    }
    ++index;
  } while (WebPDemuxNextFrame(&frame));
  WebPDemuxReleaseIterator(&frame);
  if (index != animation.inspection.frame_durations_ms.size()) throw Failure("encode_failed");
  return result;
}

struct MemoryWriter { std::vector<std::uint8_t> bytes; std::uint64_t maximum{}; bool exceeded{}; };

heif_error write_heif(heif_context *, const void *data, size_t size, void *userdata) {
  auto &writer = *static_cast<MemoryWriter *>(userdata);
  if (size > writer.maximum - writer.bytes.size()) {
    writer.exceeded = true;
    return {heif_error_Encoding_error, heif_suberror_Unspecified, "bounded output exceeded"};
  }
  const auto *begin = static_cast<const std::uint8_t *>(data);
  writer.bytes.insert(writer.bytes.end(), begin, begin + size);
  return {heif_error_Ok, heif_suberror_Unspecified, "ok"};
}

void check_heif(heif_error error, std::string code) {
  if (error.code != heif_error_Ok) throw Failure(std::move(code));
}

std::vector<std::uint8_t> encode_avif(const Image &frame, int quality,
                                      std::span<const std::byte> icc, std::uint64_t maximum) {
  std::unique_ptr<heif_context, decltype(&heif_context_free)> context(heif_context_alloc(), heif_context_free);
  if (!context) throw Failure("resource_limit");
  const heif_encoder_descriptor *descriptors[4]{};
  if (heif_get_encoder_descriptors(heif_compression_AV1, "aom", descriptors, 4) <= 0) throw Failure("capability_failed");
  heif_encoder *raw_encoder = nullptr;
  check_heif(heif_context_get_encoder(context.get(), descriptors[0], &raw_encoder), "capability_failed");
  std::unique_ptr<heif_encoder, decltype(&heif_encoder_release)> encoder(raw_encoder, heif_encoder_release);
  check_heif(heif_encoder_set_lossy_quality(encoder.get(), quality), "encode_failed");
  check_heif(heif_encoder_set_parameter_integer(encoder.get(), "threads", 1), "encode_failed");
  const bool alpha = alpha_present(frame);
  check_heif(heif_encoder_set_parameter_string(encoder.get(), "chroma", alpha ? "444" : "420"), "encode_failed");
  heif_image *raw_image = nullptr;
  const heif_chroma chroma = alpha ? heif_chroma_interleaved_RGBA : heif_chroma_interleaved_RGB;
  const int channels = alpha ? 4 : 3;
  check_heif(heif_image_create(frame.width, frame.height, heif_colorspace_RGB, chroma, &raw_image), "encode_failed");
  std::unique_ptr<heif_image, decltype(&heif_image_release)> image(raw_image, heif_image_release);
  check_heif(heif_image_add_plane(image.get(), heif_channel_interleaved, frame.width, frame.height, 8), "encode_failed");
  int stride = 0;
  std::uint8_t *pixels = heif_image_get_plane(image.get(), heif_channel_interleaved, &stride);
  if (!pixels || stride < frame.width * channels) throw Failure("encode_failed");
  for (int row = 0; row < frame.height; ++row) {
    auto *destination = pixels + static_cast<std::size_t>(row) * stride;
    const auto *source = frame.rgba.data() + static_cast<std::size_t>(row) * frame.width * 4U;
    for (int column = 0; column < frame.width; ++column) {
      std::memcpy(destination + static_cast<std::size_t>(column) * channels,
                  source + static_cast<std::size_t>(column) * 4U,
                  static_cast<std::size_t>(channels));
    }
  }
  check_heif(heif_image_set_raw_color_profile(image.get(), "prof", icc.data(), icc.size()), "encode_failed");
  check_heif(heif_context_encode_image(context.get(), image.get(), encoder.get(), nullptr, nullptr), "encode_failed");
  MemoryWriter state{.bytes = {}, .maximum = maximum, .exceeded = false};
  heif_writer writer{.writer_api_version = 1, .write = write_heif};
  const heif_error error = heif_context_write(context.get(), &writer, &state);
  if (error.code != heif_error_Ok || state.bytes.empty()) throw Failure(state.exceeded ? "output_too_large" : "encode_failed");
  return state.bytes;
}

std::string audit_json(const Animation &animation, std::string_view encoder,
                       std::string_view digest, const std::map<std::string, std::string> &versions) {
  const bool alpha = std::any_of(animation.frames.begin(), animation.frames.end(), alpha_present);
  return "{\"decoder\":" + json_string(animation.decoder) + ",\"encoder\":" + json_string(encoder) +
         ",\"tool_version\":" + json_string(kHelperVersion) + ",\"library_versions\":" + json_versions(versions) +
         ",\"icc_sha256\":" + json_string(digest) + ",\"composition\":\"composited-rgba\"" +
         ",\"timing_normalization\":\"zero-duration-to-100ms\",\"loop_normalization\":\"total-play-count\"" +
         ",\"input_color\":" + json_string(animation.embedded_icc.empty() ? "assumed-srgb" : "embedded-icc") +
         ",\"output_color\":\"srgb\",\"alpha\":" + json_string(alpha ? "preserved" : "opaque") +
         ",\"metadata\":\"strip-after-normalization-keep-color-tags\"}";
}

}  // namespace

std::map<std::string, std::string> library_versions() {
  return {{"giflib", std::to_string(GIFLIB_MAJOR) + "." + std::to_string(GIFLIB_MINOR) + "." + std::to_string(GIFLIB_RELEASE)},
          {"libwebp", webp_version(WebPGetDecoderVersion())},
          {"libwebp-demux", webp_version(WebPGetDemuxVersion())},
          {"libwebp-mux", webp_version(WebPGetMuxVersion())}, {"libheif", heif_get_version()},
          {"libaom", aom_codec_version_str()}, {"lcms2", lcms_version()}};
}

std::string capabilities_json(const CapabilityArgs &args) {
  const auto icc = read_regular_file(args.srgb_icc, 16U << 20U);
  validate_srgb(icc);
  const heif_encoder_descriptor *descriptors[1]{};
  if (heif_get_encoder_descriptors(heif_compression_AV1, "aom", descriptors, 1) != 1) throw Failure("capability_failed");
  return "{\"protocol\":1,\"ok\":true,\"error_code\":\"\",\"result\":{\"helper_version\":" +
         json_string(kHelperVersion) + ",\"library_versions\":" + json_versions(library_versions()) +
         ",\"decoder_mime_types\":[\"image/gif\",\"image/webp\"],\"encoders\":[\"animated-webp\",\"avif\"]" +
         ",\"icc_sha256\":" + json_string(sha256_hex(icc)) + ",\"threads\":" + std::to_string(args.threads) +
         ",\"build_manifest\":" + json_string(kBuildManifest) + "}}";
}

std::string inspect_json(const InspectArgs &args) {
  const Animation animation = decode(args.input, args.input_mime, args.limits, false);
  return "{\"protocol\":1,\"ok\":true,\"error_code\":\"\",\"result\":" + inspection_json(animation.inspection) + "}";
}

std::string transform_json(const TransformArgs &args) {
  const auto icc = read_regular_file(args.srgb_icc, 16U << 20U);
  validate_srgb(icc);
  Animation animation = decode(args.input, args.input_mime, args.limits, true);
  if (animation.inspection.classification != "animation") throw Failure("static_input");
  normalize_color(animation, icc);
  const auto [width, height] = resize_dimensions(animation.inspection.width, animation.inspection.height, args.max_long_edge);
  std::vector<std::uint8_t> output;
  std::string_view mime;
  std::string_view encoder;
  if (args.output_kind == "animated-webp") {
    output = encode_webp(animation, args.quality, width, height, icc, args.limits.output_bytes);
    mime = "image/webp";
    encoder = "libwebp";
  } else {
    const Image first = resize_premultiplied(animation.frames.front(), width, height);
    output = encode_avif(first, args.quality, icc, args.limits.output_bytes);
    mime = "image/avif";
    encoder = "aom";
  }
  write_all_bounded(args.output, output, args.limits.output_bytes);
  return "{\"protocol\":1,\"ok\":true,\"error_code\":\"\",\"result\":{\"output_mime\":" + json_string(mime) +
         ",\"width\":" + std::to_string(width) + ",\"height\":" + std::to_string(height) +
         ",\"quality\":" + std::to_string(args.quality) + ",\"bit_depth\":8,\"max_long_edge\":" +
         std::to_string(args.max_long_edge) + ",\"threads\":1,\"source\":" + inspection_json(animation.inspection) +
         ",\"audit\":" + audit_json(animation, encoder, sha256_hex(icc), library_versions()) + "}}";
}

}  // namespace nmcp_animation
