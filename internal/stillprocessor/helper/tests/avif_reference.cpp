// Project-owned independent AVIF decode evidence. See ../LICENSE.md.
#include <libheif/heif.h>

#include <cstdlib>
#include <fstream>
#include <iostream>
#include <memory>

namespace {

void check(heif_error error) {
  if (error.code != heif_error_Ok) {
    std::cerr << "libheif failure\n";
    std::exit(1);
  }
}

}  // namespace

int main(int argc, char **argv) {
  if (argc != 3) return 2;
  std::unique_ptr<heif_context, decltype(&heif_context_free)> context(heif_context_alloc(), heif_context_free);
  if (!context) return 1;
  heif_context_set_max_decoding_threads(context.get(), 1);
  check(heif_context_read_from_file(context.get(), argv[1], nullptr));
  heif_image_handle *raw_handle = nullptr;
  check(heif_context_get_primary_image_handle(context.get(), &raw_handle));
  std::unique_ptr<heif_image_handle, decltype(&heif_image_handle_release)> handle(raw_handle, heif_image_handle_release);
  if (!heif_image_handle_has_alpha_channel(handle.get())) return 1;
  heif_colorspace preferred_colorspace = heif_colorspace_undefined;
  heif_chroma preferred_chroma = heif_chroma_undefined;
  check(heif_image_handle_get_preferred_decoding_colorspace(handle.get(), &preferred_colorspace, &preferred_chroma));
  if (preferred_chroma != heif_chroma_444) {
    std::cerr << "primary image is not 4:4:4\n";
    return 1;
  }
  heif_decoding_options *raw_options = heif_decoding_options_alloc();
  if (!raw_options) return 1;
  std::unique_ptr<heif_decoding_options, decltype(&heif_decoding_options_free)> options(raw_options, heif_decoding_options_free);
  options->strict_decoding = 1;
  options->num_codec_threads = 1;
  heif_image *raw_image = nullptr;
  check(heif_decode_image(handle.get(), &raw_image, heif_colorspace_RGB, heif_chroma_interleaved_RGBA, options.get()));
  std::unique_ptr<heif_image, decltype(&heif_image_release)> image(raw_image, heif_image_release);
  int stride_value = 0;
  const auto *pixels = heif_image_get_plane_readonly(image.get(), heif_channel_interleaved, &stride_value);
  const int width = heif_image_get_width(image.get(), heif_channel_interleaved);
  const int height = heif_image_get_height(image.get(), heif_channel_interleaved);
  if (!pixels || stride_value < width * 4 || width <= 0 || height <= 0) return 1;
  std::ofstream output(argv[2], std::ios::binary | std::ios::trunc);
  for (int row = 0; row < height; ++row) {
    output.write(reinterpret_cast<const char *>(pixels + static_cast<std::size_t>(row) * stride_value),
                 static_cast<std::streamsize>(width * 4));
  }
  return output ? 0 : 1;
}
