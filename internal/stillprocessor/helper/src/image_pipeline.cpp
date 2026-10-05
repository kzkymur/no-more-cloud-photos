// Project-owned source. See ../LICENSE.md.
#include "nmcp/image_pipeline.h"
#include "nmcp/hdr.h"

#include <aom/aom_codec.h>
#include <fcntl.h>
#include <libheif/heif.h>
#include <libraw/libraw.h>
#include <lcms2.h>
#include <sys/stat.h>
#include <unistd.h>
#include <vips/vips.h>

#include <algorithm>
#include <array>
#include <cerrno>
#include <cmath>
#include <cstring>
#include <limits>
#include <memory>
#include <sstream>

namespace nmcp {
namespace {

struct Raster {
  int width{};
  int height{};
  bool alpha{};
  std::vector<std::uint8_t> rgba;
  Audit audit;
};

using VipsPtr = std::unique_ptr<VipsImage, decltype(&g_object_unref)>;
using ProfilePtr = std::unique_ptr<std::remove_pointer_t<cmsHPROFILE>, decltype(&cmsCloseProfile)>;

void check_heif(const heif_error error, std::string_view code = "decode_failed") {
  if (error.code != heif_error_Ok) throw Failure(std::string(code));
}

std::size_t checked_size(int width, int height, int channels, int bytes = 1) {
  if (width <= 0 || height <= 0 || channels <= 0 || bytes <= 0 || width > 1'000'000 || height > 1'000'000) {
    throw Failure("decode_failed");
  }
  const auto value = static_cast<std::uint64_t>(width) * static_cast<std::uint64_t>(height) *
                     static_cast<std::uint64_t>(channels) * static_cast<std::uint64_t>(bytes);
  if (value > std::numeric_limits<std::size_t>::max()) throw Failure("resource_limit");
  return static_cast<std::size_t>(value);
}

std::uint16_t little16(std::span<const std::byte> bytes, std::size_t offset) {
  return static_cast<std::uint16_t>(std::to_integer<unsigned>(bytes[offset])) |
         static_cast<std::uint16_t>(std::to_integer<unsigned>(bytes[offset + 1]) << 8U);
}

std::uint32_t little32(std::span<const std::byte> bytes, std::size_t offset) {
  std::uint32_t result = 0;
  for (unsigned index = 0; index != 4; ++index) {
    result |= static_cast<std::uint32_t>(std::to_integer<unsigned>(bytes[offset + index])) << (index * 8U);
  }
  return result;
}

std::int32_t signed_little32(std::span<const std::byte> bytes, std::size_t offset) {
  return static_cast<std::int32_t>(little32(bytes, offset));
}

std::vector<std::byte> read_input(const std::string &path, std::size_t maximum) {
  const int descriptor = ::open(path.c_str(), O_RDONLY | O_CLOEXEC);
  if (descriptor < 0) throw Failure("decode_failed");
  struct stat status{};
  if (fstat(descriptor, &status) != 0 || !S_ISREG(status.st_mode) || status.st_size <= 0 ||
      static_cast<std::uint64_t>(status.st_size) > maximum) {
    ::close(descriptor);
    throw Failure("resource_limit");
  }
  std::vector<std::byte> result(static_cast<std::size_t>(status.st_size));
  std::size_t offset = 0;
  while (offset != result.size()) {
    const ssize_t count = ::read(descriptor, result.data() + offset, result.size() - offset);
    if (count < 0 && errno == EINTR) continue;
    if (count <= 0) {
      ::close(descriptor);
      throw Failure("decode_failed");
    }
    offset += static_cast<std::size_t>(count);
  }
  if (::close(descriptor) != 0) throw Failure("decode_failed");
  return result;
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

std::vector<std::byte> embedded_icc(VipsImage *image) {
  const void *data = nullptr;
  size_t length = 0;
  if (vips_image_get_typeof(image, VIPS_META_ICC_NAME) == 0) return {};
  if (vips_image_get_blob(image, VIPS_META_ICC_NAME, &data, &length) != 0 || data == nullptr || length == 0 || length > 16U << 20) {
    throw Failure("decode_failed");
  }
  const auto *begin = static_cast<const std::byte *>(data);
  return {begin, begin + length};
}

void apply_icc(Raster &raster, std::span<const std::byte> source, std::span<const std::byte> target) {
  ProfilePtr source_profile(cmsOpenProfileFromMem(source.data(), static_cast<cmsUInt32Number>(source.size())), cmsCloseProfile);
  ProfilePtr target_profile(cmsOpenProfileFromMem(target.data(), static_cast<cmsUInt32Number>(target.size())), cmsCloseProfile);
  if (!source_profile || !target_profile || cmsGetColorSpace(source_profile.get()) != cmsSigRgbData ||
      cmsGetColorSpace(target_profile.get()) != cmsSigRgbData) {
    throw Failure("decode_failed");
  }
  const auto format = TYPE_RGBA_8;
  cmsHTRANSFORM transform = cmsCreateTransform(source_profile.get(), format, target_profile.get(), format,
                                                INTENT_RELATIVE_COLORIMETRIC,
                                                cmsFLAGS_COPY_ALPHA | cmsFLAGS_BLACKPOINTCOMPENSATION);
  if (transform == nullptr) throw Failure("processing_failed");
  cmsDoTransform(transform, raster.rgba.data(), raster.rgba.data(),
                 static_cast<cmsUInt32Number>(checked_size(raster.width, raster.height, 1)));
  cmsDeleteTransform(transform);
}

VipsPtr autorotate(VipsPtr input) {
  VipsImage *rotated = nullptr;
  if (vips_autorot(input.get(), &rotated, nullptr) != 0) throw Failure("decode_failed");
  return VipsPtr(rotated, g_object_unref);
}

Raster raster_from_vips(VipsPtr image, std::string decoder, std::span<const std::byte> target_icc) {
  auto source_icc = embedded_icc(image.get());
  image = autorotate(std::move(image));
  VipsImage *colour = nullptr;
  if (vips_colourspace(image.get(), &colour, VIPS_INTERPRETATION_sRGB, nullptr) != 0) throw Failure("decode_failed");
  VipsPtr normalized(colour, g_object_unref);
  VipsImage *cast = nullptr;
  if (vips_cast(normalized.get(), &cast, VIPS_FORMAT_UCHAR, nullptr) != 0) throw Failure("decode_failed");
  VipsPtr bytes(cast, g_object_unref);
  const int bands = vips_image_get_bands(bytes.get());
  if (bands != 3 && bands != 4) throw Failure("decode_failed");
  size_t memory_size = 0;
  void *memory = vips_image_write_to_memory(bytes.get(), &memory_size);
  if (memory == nullptr || memory_size != checked_size(bytes->Xsize, bytes->Ysize, bands)) {
    g_free(memory);
    throw Failure("decode_failed");
  }
  std::unique_ptr<void, decltype(&g_free)> owned(memory, g_free);
  Raster raster{.width = bytes->Xsize, .height = bytes->Ysize, .alpha = bands == 4};
  raster.rgba.resize(checked_size(raster.width, raster.height, 4));
  const auto *pixels = static_cast<const std::uint8_t *>(memory);
  for (std::size_t input = 0, output = 0; input < memory_size; input += static_cast<std::size_t>(bands), output += 4) {
    raster.rgba[output] = pixels[input];
    raster.rgba[output + 1] = pixels[input + 1];
    raster.rgba[output + 2] = pixels[input + 2];
    raster.rgba[output + 3] = bands == 4 ? pixels[input + 3] : 255;
  }
  raster.audit = {.decoder = std::move(decoder),
                   .input_color = source_icc.empty() ? "assumed-srgb" : "embedded-icc",
                   .input_primaries = source_icc.empty() ? "srgb" : "profile-defined",
                   .input_transfer = source_icc.empty() ? "srgb" : "profile-defined",
                   .alpha = raster.alpha ? "preserved" : "opaque",
                  .chroma = raster.alpha ? "4:4:4" : "4:2:0",
                  .source_width = raster.width, .source_height = raster.height};
  if (!source_icc.empty()) apply_icc(raster, source_icc, target_icc);
  return raster;
}

Raster decode_vips(const TransformArgs &args, std::span<const std::byte> target_icc) {
  VipsImage *raw = nullptr;
  std::string decoder;
  int result = -1;
  if (args.input_mime == "image/jpeg") {
    decoder = "libvips-jpeg";
    result = vips_jpegload(args.input.c_str(), &raw, "access", VIPS_ACCESS_SEQUENTIAL, nullptr);
  } else if (args.input_mime == "image/png") {
    decoder = "libvips-png";
    result = vips_pngload(args.input.c_str(), &raw, "access", VIPS_ACCESS_SEQUENTIAL, nullptr);
  } else if (args.input_mime == "image/webp") {
    decoder = "libvips-webp";
    result = vips_webpload(args.input.c_str(), &raw, "access", VIPS_ACCESS_SEQUENTIAL, nullptr);
  } else {
    throw Failure("unsupported_input");
  }
  if (result != 0 || raw == nullptr) throw Failure("decode_failed");
  VipsPtr image(raw, g_object_unref);
  int pages = 1;
  if (vips_image_get_typeof(image.get(), "n-pages") != 0 && vips_image_get_int(image.get(), "n-pages", &pages) != 0) {
    throw Failure("decode_failed");
  }
  if (pages != 1) throw Failure("animated_input");
  return raster_from_vips(std::move(image), std::move(decoder), target_icc);
}

Raster decode_bmp(const TransformArgs &args) {
  constexpr std::size_t maximum_bmp_bytes = 512U << 20;
  const auto bytes = read_input(args.input, maximum_bmp_bytes);
  if (bytes.size() < 54 || little32(bytes, 14) != 40 || little16(bytes, 26) != 1 || little32(bytes, 30) != 0 ||
      little32(bytes, 46) != 0) {
    throw Failure("unsupported_input");
  }
  const int bits = validate_bmp(std::span(bytes).first(54));
  const std::int32_t signed_width = signed_little32(bytes, 18);
  const std::int32_t signed_height = signed_little32(bytes, 22);
  if (signed_width <= 0 || signed_height == 0 || signed_height == std::numeric_limits<std::int32_t>::min()) {
    throw Failure("unsupported_input");
  }
  const int width = signed_width;
  const int height = std::abs(signed_height);
  (void)checked_size(width, height, 4);
  const std::uint64_t row_bits = static_cast<std::uint64_t>(width) * static_cast<unsigned>(bits);
  const std::uint64_t stride = ((row_bits + 31U) / 32U) * 4U;
  const std::uint64_t pixel_offset = little32(bytes, 10);
  const std::uint64_t pixel_bytes = stride * static_cast<std::uint64_t>(height);
  if (pixel_offset < 54 || pixel_offset > bytes.size() || pixel_bytes > bytes.size() - pixel_offset) {
    throw Failure("decode_failed");
  }
  const std::uint32_t declared_size = little32(bytes, 2);
  const std::uint32_t declared_pixels = little32(bytes, 34);
  if ((declared_size != 0 && declared_size != bytes.size()) ||
      (declared_pixels != 0 && declared_pixels != pixel_bytes)) {
    throw Failure("decode_failed");
  }
  Raster raster{.width = width, .height = height, .alpha = false};
  raster.rgba.resize(checked_size(width, height, 4));
  const int pixel_size = bits / 8;
  for (int output_row = 0; output_row < height; ++output_row) {
    const int input_row = signed_height < 0 ? output_row : height - output_row - 1;
    const auto input = pixel_offset + static_cast<std::uint64_t>(input_row) * stride;
    for (int column = 0; column < width; ++column) {
      const auto source = input + static_cast<std::uint64_t>(column) * static_cast<unsigned>(pixel_size);
      const auto destination = (static_cast<std::size_t>(output_row) * width + column) * 4U;
      raster.rgba[destination] = std::to_integer<std::uint8_t>(bytes[source + 2]);
      raster.rgba[destination + 1] = std::to_integer<std::uint8_t>(bytes[source + 1]);
      raster.rgba[destination + 2] = std::to_integer<std::uint8_t>(bytes[source]);
      // BI_RGB's fourth byte is reserved, not alpha. Always emit opaque pixels.
      raster.rgba[destination + 3] = 255;
    }
  }
  raster.audit = {.decoder = bits == 24 ? "nmcp-bmp-bi-rgb-24" : "nmcp-bmp-bi-rgb-32",
                  .input_color = "assumed-srgb", .input_primaries = "srgb", .input_transfer = "srgb",
                  .alpha = "opaque", .chroma = "4:2:0",
                  .source_width = width, .source_height = height};
  return raster;
}

bool raw_mime(std::string_view mime) {
  static constexpr std::array<std::string_view, 8> values = {"image/dng", "image/x-canon-cr2", "image/x-canon-cr3",
      "image/x-fuji-raf", "image/x-nikon-nef", "image/x-olympus-orf", "image/x-panasonic-rw2", "image/x-sony-arw"};
  return std::find(values.begin(), values.end(), mime) != values.end();
}

std::string raw_decoder(std::string_view mime) {
  const auto slash = mime == "image/dng" ? std::string_view("dng") : mime.substr(mime.rfind('-') + 1);
  return "libraw-" + std::string(slash);
}

Raster decode_raw(const TransformArgs &args) {
  std::unique_ptr<libraw_data_t, decltype(&libraw_close)> raw(libraw_init(0), libraw_close);
  if (!raw) throw Failure("resource_limit");
  raw->params.use_camera_wb = 1;
  raw->params.use_auto_wb = 0;
  raw->params.use_camera_matrix = 1;
  raw->params.output_bps = 16;
  raw->params.no_auto_bright = 1;
  raw->params.output_color = 1;  // LibRaw's documented sRGB output matrix.
  if (libraw_open_file(raw.get(), args.input.c_str()) != LIBRAW_SUCCESS || libraw_unpack(raw.get()) != LIBRAW_SUCCESS ||
      libraw_dcraw_process(raw.get()) != LIBRAW_SUCCESS) throw Failure("decode_failed");
  int error = LIBRAW_SUCCESS;
  std::unique_ptr<libraw_processed_image_t, decltype(&libraw_dcraw_clear_mem)> image(
      libraw_dcraw_make_mem_image(raw.get(), &error), libraw_dcraw_clear_mem);
  if (!image || error != LIBRAW_SUCCESS || image->type != LIBRAW_IMAGE_BITMAP || image->bits != 16 || image->colors != 3) {
    throw Failure("decode_failed");
  }
  Raster raster{.width = static_cast<int>(image->width), .height = static_cast<int>(image->height), .alpha = false};
  const auto samples = checked_size(raster.width, raster.height, 3);
  if (image->data_size != samples * sizeof(std::uint16_t)) throw Failure("decode_failed");
  raster.rgba.resize(checked_size(raster.width, raster.height, 4));
  for (std::size_t sample = 0, output = 0; sample < samples; sample += 3, output += 4) {
    std::array<std::uint16_t, 3> rgb{};
    std::memcpy(rgb.data(), image->data + sample * sizeof(std::uint16_t), 3 * sizeof(std::uint16_t));
    raster.rgba[output] = static_cast<std::uint8_t>((rgb[0] + 128U) / 257U);
    raster.rgba[output + 1] = static_cast<std::uint8_t>((rgb[1] + 128U) / 257U);
    raster.rgba[output + 2] = static_cast<std::uint8_t>((rgb[2] + 128U) / 257U);
    raster.rgba[output + 3] = 255;
  }
  raster.audit = {.decoder = raw_decoder(args.input_mime), .input_color = "raw-camera-matrix",
                  .input_primaries = "camera-matrix", .input_transfer = "libraw-srgb",
                  .raw_processing = "camera-wb-camera-matrix-16bit-no-auto-bright", .alpha = "opaque", .chroma = "4:2:0",
                  .source_width = raster.width, .source_height = raster.height};
  return raster;
}

Raster decode_heif(const TransformArgs &args, std::span<const std::byte> target_icc) {
  std::unique_ptr<heif_context, decltype(&heif_context_free)> context(heif_context_alloc(), heif_context_free);
  if (!context) throw Failure("resource_limit");
  heif_context_set_max_decoding_threads(context.get(), 1);
  check_heif(heif_context_read_from_file(context.get(), args.input.c_str(), nullptr));
  if (heif_context_get_number_of_top_level_images(context.get()) != 1) throw Failure("animated_input");
  heif_image_handle *raw_handle = nullptr;
  check_heif(heif_context_get_primary_image_handle(context.get(), &raw_handle));
  std::unique_ptr<heif_image_handle, decltype(&heif_image_handle_release)> handle(raw_handle, heif_image_handle_release);
  std::vector<std::byte> source_icc;
  const auto profile_type = heif_image_handle_get_color_profile_type(handle.get());
  if (profile_type == heif_color_profile_type_rICC || profile_type == heif_color_profile_type_prof) {
    const auto size = heif_image_handle_get_raw_color_profile_size(handle.get());
    if (size == 0 || size > 16U << 20) throw Failure("decode_failed");
    source_icc.resize(size);
    check_heif(heif_image_handle_get_raw_color_profile(handle.get(), source_icc.data()));
  }
  heif_color_profile_nclx *raw_nclx = nullptr;
  const auto nclx_error = heif_image_handle_get_nclx_color_profile(handle.get(), &raw_nclx);
  std::unique_ptr<heif_color_profile_nclx, decltype(&heif_nclx_color_profile_free)> nclx(
      nclx_error.code == heif_error_Ok ? raw_nclx : nullptr, heif_nclx_color_profile_free);
  if (nclx_error.code != heif_error_Ok && nclx_error.code != heif_error_Color_profile_does_not_exist) {
    throw Failure("decode_failed");
  }
  const bool pq = nclx && nclx->transfer_characteristics == heif_transfer_characteristic_ITU_R_BT_2100_0_PQ;
  const bool hlg = nclx && nclx->transfer_characteristics == heif_transfer_characteristic_ITU_R_BT_2100_0_HLG;
  const bool hdr = pq || hlg;
  if (hdr && (!source_icc.empty() || nclx->color_primaries != heif_color_primaries_ITU_R_BT_2020_2_and_2100_0 ||
              nclx->matrix_coefficients != heif_matrix_coefficients_ITU_R_BT_2020_2_non_constant_luminance)) {
    throw Failure("unsupported_input");
  }
  const bool nclx_sdr = nclx && !hdr && nclx->color_primaries == heif_color_primaries_ITU_R_BT_709_5 &&
      (nclx->transfer_characteristics == heif_transfer_characteristic_ITU_R_BT_709_5 ||
       nclx->transfer_characteristics == heif_transfer_characteristic_IEC_61966_2_1) &&
      (nclx->matrix_coefficients == heif_matrix_coefficients_ITU_R_BT_709_5 ||
       nclx->matrix_coefficients == heif_matrix_coefficients_RGB_GBR);
  if (nclx && !hdr && !nclx_sdr && source_icc.empty()) throw Failure("unsupported_input");

  heif_decoding_options *options = heif_decoding_options_alloc();
  if (!options) throw Failure("resource_limit");
  options->ignore_transformations = 0;
  options->strict_decoding = 1;
  // Keep source NCLX semantics. The libheif default converts an unspecified
  // output profile to sRGB, which would make us interpret already-converted
  // pixels as PQ/HLG below. Codec threading is independent of tile threading.
  options->output_image_nclx_profile = nullptr;
  options->output_image_nclx_profile_passthrough = 1;
  options->num_codec_threads = 1;
  heif_image *raw_image = nullptr;
  const bool alpha = heif_image_handle_has_alpha_channel(handle.get()) != 0;
  const auto chroma = hdr ? (alpha ? heif_chroma_interleaved_RRGGBBAA_BE : heif_chroma_interleaved_RRGGBB_BE)
                          : (alpha ? heif_chroma_interleaved_RGBA : heif_chroma_interleaved_RGB);
  const auto decode_error = heif_decode_image(handle.get(), &raw_image, heif_colorspace_RGB, chroma, options);
  heif_decoding_options_free(options);
  check_heif(decode_error);
  std::unique_ptr<heif_image, decltype(&heif_image_release)> image(raw_image, heif_image_release);
  int stride_value = 0;
  const auto *pixels = heif_image_get_plane_readonly(image.get(), heif_channel_interleaved, &stride_value);
  if (stride_value < 0) throw Failure("decode_failed");
  const auto stride = static_cast<std::size_t>(stride_value);
  const int width = heif_image_get_width(image.get(), heif_channel_interleaved);
  const int height = heif_image_get_height(image.get(), heif_channel_interleaved);
  const int channels = alpha ? 4 : 3;
  const int bytes_per_channel = hdr ? 2 : 1;
  if (!pixels || stride < static_cast<std::size_t>(width) * static_cast<unsigned>(channels * bytes_per_channel)) {
    throw Failure("decode_failed");
  }
  Raster raster{.width = width, .height = height, .alpha = alpha};
  raster.rgba.resize(checked_size(width, height, 4));
  if (hdr) {
    const int bits = heif_image_handle_get_luma_bits_per_pixel(handle.get());
    if (bits < 10 || bits > 16) throw Failure("unsupported_input");
    const double maximum = static_cast<double>((std::uint64_t{1} << bits) - 1U);
    for (int row = 0; row < height; ++row) {
      const auto *source = pixels + static_cast<std::size_t>(row) * stride;
      for (int column = 0; column < width; ++column) {
        const auto input = static_cast<std::size_t>(column) * static_cast<unsigned>(channels) * 2U;
        const auto sample = [source, input, maximum](int channel) {
          const auto offset = input + static_cast<std::size_t>(channel) * 2U;
          const auto value = static_cast<unsigned>(source[offset]) << 8U | source[offset + 1];
          return std::clamp(static_cast<double>(value) / maximum, 0.0, 1.0);
        };
        const auto mapped = tone_map_bt2446a(sample(0), sample(1), sample(2), pq ? HdrTransfer::pq : HdrTransfer::hlg);
        const auto output = (static_cast<std::size_t>(row) * width + column) * 4U;
        raster.rgba[output] = mapped[0];
        raster.rgba[output + 1] = mapped[1];
        raster.rgba[output + 2] = mapped[2];
        raster.rgba[output + 3] = alpha ? static_cast<std::uint8_t>(std::lround(sample(3) * 255.0)) : 255;
      }
    }
  } else {
    for (int row = 0; row < height; ++row) {
      const auto *source = pixels + static_cast<std::size_t>(row) * stride;
      for (int column = 0; column < width; ++column) {
        const auto input = static_cast<std::size_t>(column) * static_cast<unsigned>(channels);
        const auto output = (static_cast<std::size_t>(row) * width + column) * 4U;
        raster.rgba[output] = source[input];
        raster.rgba[output + 1] = source[input + 1];
        raster.rgba[output + 2] = source[input + 2];
        raster.rgba[output + 3] = alpha ? source[input + 3] : 255;
      }
    }
  }
  raster.audit = {.decoder = args.input_mime == "image/heic" ? "libheif-heic" : "libheif-heif",
                   .input_color = hdr ? (pq ? "nclx-pq" : "nclx-hlg") :
                       (!source_icc.empty() ? "embedded-icc" : (nclx_sdr ? "nclx-sdr" : "assumed-srgb")),
                   .input_primaries = hdr ? "bt2020" : (!source_icc.empty() ? "profile-defined" : (nclx_sdr ? "bt709" : "srgb")),
                   .input_transfer = hdr ? (pq ? "pq" : "hlg") : (!source_icc.empty() ? "profile-defined" :
                       (nclx_sdr && nclx->transfer_characteristics == heif_transfer_characteristic_ITU_R_BT_709_5 ? "bt709" : "srgb")),
                   .input_range = (hdr || (nclx_sdr && source_icc.empty())) ?
                       (nclx->full_range_flag ? "full" : "limited") : "not-applicable",
                   .hdr_disposition = hdr ? "tone-mapped" : "sdr",
                   .tone_map = hdr ? "bt2446a-method-a" : "not-needed",
                   .target_nits = hdr ? 100 : 0, .hdr_peak_nits = hdr ? 1000 : 0,
                   .hlg_reference_nits = hlg ? 1000 : 0,
                   .alpha = raster.alpha ? "preserved" : "opaque", .chroma = raster.alpha ? "4:4:4" : "4:2:0",
                   .source_width = width, .source_height = height};
  if (!source_icc.empty()) apply_icc(raster, source_icc, target_icc);
  return raster;
}

Raster resize_raster(Raster raster, int maximum) {
  const auto [width, height] = resize_dimensions(raster.width, raster.height, maximum);
  if (width == raster.width && height == raster.height) return raster;
  if (raster.alpha) {
    for (std::size_t offset = 0; offset < raster.rgba.size(); offset += 4) {
      const unsigned alpha = raster.rgba[offset + 3];
      for (std::size_t channel = 0; channel < 3; ++channel) {
        raster.rgba[offset + channel] = static_cast<std::uint8_t>(
            (static_cast<unsigned>(raster.rgba[offset + channel]) * alpha + 127U) / 255U);
      }
    }
  }
  VipsImage *source_raw = vips_image_new_from_memory(raster.rgba.data(), raster.rgba.size(), raster.width, raster.height, 4, VIPS_FORMAT_UCHAR);
  if (!source_raw) throw Failure("processing_failed");
  VipsPtr source(source_raw, g_object_unref);
  VipsImage *resized_raw = nullptr;
  const double horizontal = static_cast<double>(width) / raster.width;
  const double vertical = static_cast<double>(height) / raster.height;
  if (vips_resize(source.get(), &resized_raw, horizontal, "vscale", vertical, "kernel", VIPS_KERNEL_LANCZOS3, nullptr) != 0) {
    throw Failure("processing_failed");
  }
  VipsPtr resized(resized_raw, g_object_unref);
  size_t size = 0;
  void *memory = vips_image_write_to_memory(resized.get(), &size);
  if (!memory || size != checked_size(width, height, 4)) {
    g_free(memory);
    throw Failure("processing_failed");
  }
  const auto *begin = static_cast<const std::uint8_t *>(memory);
  raster.rgba.assign(begin, begin + size);
  g_free(memory);
  if (raster.alpha) {
    for (std::size_t offset = 0; offset < raster.rgba.size(); offset += 4) {
      const unsigned alpha = raster.rgba[offset + 3];
      for (std::size_t channel = 0; channel < 3; ++channel) {
        raster.rgba[offset + channel] = alpha == 0 ? 0 : static_cast<std::uint8_t>(std::min(
            255U, (static_cast<unsigned>(raster.rgba[offset + channel]) * 255U + alpha / 2U) / alpha));
      }
    }
  }
  raster.width = width;
  raster.height = height;
  return raster;
}

struct BoundedWriter {
  int descriptor{-1};
  std::uint64_t maximum{};
  std::uint64_t written{};
  bool failed{};
  bool exceeded{};
};

heif_error write_heif(heif_context *, const void *data, size_t size, void *userdata) {
  auto &writer = *static_cast<BoundedWriter *>(userdata);
  if (size > writer.maximum - writer.written) {
    writer.failed = true;
    writer.exceeded = true;
    return {heif_error_Encoding_error, heif_suberror_Unspecified, "bounded output exceeded"};
  }
  const auto *bytes = static_cast<const std::byte *>(data);
  std::size_t offset = 0;
  while (offset != size) {
    const ssize_t count = ::write(writer.descriptor, bytes + offset, size - offset);
    if (count < 0 && errno == EINTR) continue;
    if (count <= 0) {
      writer.failed = true;
      return {heif_error_Encoding_error, heif_suberror_Unspecified, "output write failed"};
    }
    offset += static_cast<std::size_t>(count);
  }
  writer.written += size;
  return {heif_error_Ok, heif_suberror_Unspecified, "ok"};
}

void encode_avif(const Raster &raster, const TransformArgs &args, std::span<const std::byte> icc) {
  std::unique_ptr<heif_context, decltype(&heif_context_free)> context(heif_context_alloc(), heif_context_free);
  if (!context) throw Failure("resource_limit");
  const heif_encoder_descriptor *descriptors[8]{};
  const int count = heif_get_encoder_descriptors(heif_compression_AV1, "aom", descriptors, 8);
  if (count <= 0) throw Failure("capability_failed");
  heif_encoder *raw_encoder = nullptr;
  check_heif(heif_context_get_encoder(context.get(), descriptors[0], &raw_encoder), "capability_failed");
  std::unique_ptr<heif_encoder, decltype(&heif_encoder_release)> encoder(raw_encoder, heif_encoder_release);
  check_heif(heif_encoder_set_lossy_quality(encoder.get(), args.quality), "encode_failed");
  check_heif(heif_encoder_set_parameter_integer(encoder.get(), "threads", 1), "encode_failed");
  check_heif(heif_encoder_set_parameter_string(encoder.get(), "chroma", raster.alpha ? "444" : "420"), "encode_failed");
  const auto chroma = raster.alpha ? heif_chroma_interleaved_RGBA : heif_chroma_interleaved_RGB;
  const int channels = raster.alpha ? 4 : 3;
  heif_image *raw_image = nullptr;
  check_heif(heif_image_create(raster.width, raster.height, heif_colorspace_RGB, chroma, &raw_image), "encode_failed");
  std::unique_ptr<heif_image, decltype(&heif_image_release)> image(raw_image, heif_image_release);
  check_heif(heif_image_add_plane(image.get(), heif_channel_interleaved, raster.width, raster.height, 8), "encode_failed");
  int stride_value = 0;
  auto *pixels = heif_image_get_plane(image.get(), heif_channel_interleaved, &stride_value);
  if (stride_value < 0) throw Failure("encode_failed");
  const auto stride = static_cast<std::size_t>(stride_value);
  if (!pixels || stride < static_cast<std::size_t>(raster.width) * channels) throw Failure("encode_failed");
  for (int row = 0; row < raster.height; ++row) {
    auto *destination = pixels + static_cast<std::size_t>(row) * stride;
    const auto *source = raster.rgba.data() + static_cast<std::size_t>(row) * raster.width * 4;
    for (int column = 0; column < raster.width; ++column) {
      std::memcpy(destination + static_cast<std::size_t>(column) * channels,
                  source + static_cast<std::size_t>(column) * 4, static_cast<std::size_t>(channels));
    }
  }
  check_heif(heif_image_set_raw_color_profile(image.get(), "prof", icc.data(), icc.size()), "encode_failed");
  check_heif(heif_context_encode_image(context.get(), image.get(), encoder.get(), nullptr, nullptr), "encode_failed");
  const int descriptor = ::open(args.output.c_str(), O_WRONLY | O_CLOEXEC);
  if (descriptor < 0) throw Failure("policy_violation");
  struct stat status{};
  if (fstat(descriptor, &status) != 0 || !S_ISREG(status.st_mode) || status.st_size != 0 || lseek(descriptor, 0, SEEK_CUR) != 0) {
    ::close(descriptor);
    throw Failure("policy_violation");
  }
  BoundedWriter state{.descriptor = descriptor, .maximum = args.max_output_bytes};
  heif_writer writer{.writer_api_version = 1, .write = write_heif};
  const auto error = heif_context_write(context.get(), &writer, &state);
  const bool failed = error.code != heif_error_Ok || state.failed || state.written == 0;
  if (failed && ::ftruncate(descriptor, 0) != 0) state.failed = true;
  const bool close_failed = ::close(descriptor) != 0;
  if (failed || close_failed) {
    throw Failure(state.exceeded ? "output_too_large" : "encode_failed");
  }
}

std::string lcms_version() {
  const unsigned version = cmsGetEncodedCMMversion();
  return std::to_string(version / 1000U) + "." + std::to_string((version / 10U) % 100U);
}

}  // namespace

std::vector<std::byte> read_regular_file(const std::string &path, std::size_t maximum) {
  const int descriptor = ::open(path.c_str(), O_RDONLY | O_CLOEXEC);
  if (descriptor < 0) throw Failure("policy_violation");
  struct stat status{};
  if (fstat(descriptor, &status) != 0 || !S_ISREG(status.st_mode) || status.st_size <= 0 ||
      static_cast<std::uint64_t>(status.st_size) > maximum) {
    ::close(descriptor);
    throw Failure("policy_violation");
  }
  std::vector<std::byte> result(static_cast<std::size_t>(status.st_size));
  std::size_t offset = 0;
  while (offset != result.size()) {
    const ssize_t count = ::read(descriptor, result.data() + offset, result.size() - offset);
    if (count < 0 && errno == EINTR) continue;
    if (count <= 0) {
      ::close(descriptor);
      throw Failure("policy_violation");
    }
    offset += static_cast<std::size_t>(count);
  }
  if (::close(descriptor) != 0) throw Failure("policy_violation");
  return result;
}

void validate_srgb_profile(std::span<const std::byte> profile) {
  ProfilePtr parsed(cmsOpenProfileFromMem(profile.data(), static_cast<cmsUInt32Number>(profile.size())), cmsCloseProfile);
  if (!parsed || cmsGetColorSpace(parsed.get()) != cmsSigRgbData || cmsGetPCS(parsed.get()) != cmsSigXYZData) {
    throw Failure("capability_failed");
  }
}

std::map<std::string, std::string> library_versions() {
  return {{"libaom", aom_codec_version_str()}, {"libheif", heif_get_version()}, {"libraw", libraw_version()},
          {"libvips", vips_version_string()}, {"lcms2", lcms_version()}};
}

std::vector<std::string> decoder_mime_types() {
  std::vector<std::string> result;
  result.emplace_back("image/bmp");
  // LibRaw's linked decoder set owns the same eight-format closed dispatch.
  result.emplace_back("image/dng");
  const heif_decoder_descriptor *hevc[1]{};
  const heif_decoder_descriptor *av1[1]{};
  const bool has_hevc = heif_get_decoder_descriptors(heif_compression_HEVC, hevc, 1) == 1;
  const bool has_av1 = heif_get_decoder_descriptors(heif_compression_AV1, av1, 1) == 1;
  if (has_hevc) result.emplace_back("image/heic");
  if (has_hevc || has_av1) result.emplace_back("image/heif");
  if (vips_type_find("VipsOperation", "jpegload") != 0) result.emplace_back("image/jpeg");
  if (vips_type_find("VipsOperation", "pngload") != 0) result.emplace_back("image/png");
  if (vips_type_find("VipsOperation", "webpload") != 0) result.emplace_back("image/webp");
  result.insert(result.end(), {"image/x-canon-cr2", "image/x-canon-cr3", "image/x-fuji-raf", "image/x-nikon-nef",
                               "image/x-olympus-orf", "image/x-panasonic-rw2", "image/x-sony-arw"});
  std::sort(result.begin(), result.end());
  return result;
}

bool aom_encoder_available() {
  const heif_encoder_descriptor *descriptors[1]{};
  return heif_get_encoder_descriptors(heif_compression_AV1, "aom", descriptors, 1) == 1;
}

TransformResult transform_image(const TransformArgs &args, std::span<const std::byte> srgb_icc) {
  if ((args.input_mime == "image/webp" && args.source_mode != "probe-animation") ||
      (args.input_mime != "image/webp" && args.source_mode != "still")) {
    throw Failure("policy_violation");
  }
  Raster raster;
  if (raw_mime(args.input_mime)) raster = decode_raw(args);
  else if (args.input_mime == "image/heic" || args.input_mime == "image/heif") raster = decode_heif(args, srgb_icc);
  else if (args.input_mime == "image/bmp") raster = decode_bmp(args);
  else raster = decode_vips(args, srgb_icc);
  const int source_width = raster.audit.source_width;
  const int source_height = raster.audit.source_height;
  raster = resize_raster(std::move(raster), args.max_long_edge);
  encode_avif(raster, args, srgb_icc);
  raster.audit.source_width = source_width;
  raster.audit.source_height = source_height;
  return {.width = raster.width, .height = raster.height, .audit = std::move(raster.audit)};
}

std::string capabilities_json(const CapabilityArgs &args) {
  const auto icc = read_regular_file(args.srgb_icc, 16U << 20);
  validate_srgb_profile(icc);
  if (!aom_encoder_available()) throw Failure("capability_failed");
  const auto versions = library_versions();
  const auto mimes = decoder_mime_types();
  std::ostringstream output;
  output << "{\"protocol\":1,\"ok\":true,\"error_code\":\"\",\"result\":{\"helper_version\":"
         << json_string(kHelperVersion) << ",\"library_versions\":" << json_versions(versions) << ",\"decoder_mime_types\":[";
  for (std::size_t index = 0; index < mimes.size(); ++index) {
    if (index != 0) output << ',';
    output << json_string(mimes[index]);
  }
  output << "],\"avif_encoder\":\"aom\",\"icc_sha256\":" << json_string(sha256_hex(icc))
         << ",\"threads\":" << args.threads << "}}";
  return output.str();
}

std::string transform_json(const TransformArgs &args) {
  const auto icc = read_regular_file(args.srgb_icc, 16U << 20);
  validate_srgb_profile(icc);
  if (!aom_encoder_available()) throw Failure("capability_failed");
  const auto result = transform_image(args, icc);
  const auto versions = library_versions();
  const auto &audit = result.audit;
  std::ostringstream output;
  output << "{\"protocol\":1,\"ok\":true,\"error_code\":\"\",\"result\":{"
         << "\"output_mime\":\"image/avif\",\"width\":" << result.width << ",\"height\":" << result.height
         << ",\"quality\":" << args.quality << ",\"bit_depth\":8,\"max_long_edge\":" << args.max_long_edge
         << ",\"threads\":1,\"audit\":{\"decoder\":" << json_string(audit.decoder)
         << ",\"encoder\":\"aom\",\"tool_version\":" << json_string(kHelperVersion)
         << ",\"library_versions\":" << json_versions(versions) << ",\"icc_sha256\":" << json_string(sha256_hex(icc))
          << ",\"orientation\":\"applied\",\"source_width\":" << audit.source_width
          << ",\"source_height\":" << audit.source_height << ",\"input_color\":" << json_string(audit.input_color)
          << ",\"input_primaries\":" << json_string(audit.input_primaries)
          << ",\"input_transfer\":" << json_string(audit.input_transfer)
          << ",\"input_range\":" << json_string(audit.input_range)
          << ",\"output_color\":\"srgb\",\"output_transfer\":\"srgb\",\"hdr_disposition\":" << json_string(audit.hdr_disposition)
          << ",\"tone_map\":" << json_string(audit.tone_map) << ",\"target_nits\":" << audit.target_nits
          << ",\"hdr_peak_nits\":" << audit.hdr_peak_nits << ",\"hlg_reference_nits\":" << audit.hlg_reference_nits
          << ",\"raw_processing\":" << json_string(audit.raw_processing)
         << ",\"alpha\":" << json_string(audit.alpha)
         << ",\"metadata\":\"strip-after-normalization-keep-color-tags\",\"chroma\":" << json_string(audit.chroma)
         << "}}}";
  return output.str();
}

}  // namespace nmcp
