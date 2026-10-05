// Project-owned test evidence. See ../LICENSE.md.
#include <libraw/libraw.h>
#include <vips/vips.h>

#include <algorithm>
#include <array>
#include <cstdint>
#include <cstdlib>
#include <cstring>
#include <fstream>
#include <iostream>
#include <memory>
#include <vector>

namespace {

[[noreturn]] void fail(const char *message) {
  std::cerr << message << '\n';
  std::exit(1);
}

int positive(const char *value) {
  char *end = nullptr;
  const long parsed = std::strtol(value, &end, 10);
  if (!end || *end != '\0' || parsed <= 0 || parsed > 1'000'000) fail("invalid dimension");
  return static_cast<int>(parsed);
}

}  // namespace

int main(int argc, char **argv) {
  if (argc != 5 || VIPS_INIT(argv[0]) != 0) return 2;
  const int output_width = positive(argv[3]);
  const int output_height = positive(argv[4]);
  std::unique_ptr<libraw_data_t, decltype(&libraw_close)> raw(libraw_init(0), libraw_close);
  if (!raw) fail("libraw init");

  // This independent oracle intentionally spells out the accepted appearance
  // policy. The integration test compares its decoded pixels with production,
  // so removing any production setting changes the evidence rather than merely
  // changing a self-reported audit string.
  raw->params.use_camera_wb = 1;
  raw->params.use_auto_wb = 0;
  raw->params.use_camera_matrix = 1;
  raw->params.output_bps = 16;
  raw->params.no_auto_bright = 1;
  raw->params.output_color = 1;
  if (libraw_open_file(raw.get(), argv[1]) != LIBRAW_SUCCESS || libraw_unpack(raw.get()) != LIBRAW_SUCCESS ||
      libraw_dcraw_process(raw.get()) != LIBRAW_SUCCESS) fail("libraw process");
  int error = LIBRAW_SUCCESS;
  std::unique_ptr<libraw_processed_image_t, decltype(&libraw_dcraw_clear_mem)> image(
      libraw_dcraw_make_mem_image(raw.get(), &error), libraw_dcraw_clear_mem);
  if (!image || error != LIBRAW_SUCCESS || image->type != LIBRAW_IMAGE_BITMAP || image->bits != 16 || image->colors != 3) {
    fail("libraw bitmap");
  }
  const std::size_t pixels = static_cast<std::size_t>(image->width) * image->height;
  if (image->data_size != pixels * 3U * sizeof(std::uint16_t)) fail("libraw size");
  std::vector<std::uint8_t> rgb(pixels * 3U);
  for (std::size_t pixel = 0; pixel < pixels; ++pixel) {
    std::array<std::uint16_t, 3> sample{};
    std::memcpy(sample.data(), image->data + pixel * 3U * sizeof(std::uint16_t), sample.size() * sizeof(std::uint16_t));
    for (std::size_t channel = 0; channel < sample.size(); ++channel) {
      rgb[pixel * 3U + channel] = static_cast<std::uint8_t>((static_cast<unsigned>(sample[channel]) + 128U) / 257U);
    }
  }

  VipsImage *source = vips_image_new_from_memory(rgb.data(), rgb.size(), image->width, image->height, 3, VIPS_FORMAT_UCHAR);
  if (!source) fail("vips source");
  VipsImage *resized = nullptr;
  const double horizontal = static_cast<double>(output_width) / image->width;
  const double vertical = static_cast<double>(output_height) / image->height;
  if (vips_resize(source, &resized, horizontal, "vscale", vertical, "kernel", VIPS_KERNEL_LANCZOS3, nullptr) != 0) {
    g_object_unref(source);
    fail("vips resize");
  }
  size_t size = 0;
  void *memory = vips_image_write_to_memory(resized, &size);
  g_object_unref(resized);
  g_object_unref(source);
  if (!memory || size != static_cast<std::size_t>(output_width) * output_height * 3U) {
    g_free(memory);
    fail("vips output");
  }
  std::ofstream output(argv[2], std::ios::binary | std::ios::trunc);
  output.write(static_cast<const char *>(memory), static_cast<std::streamsize>(size));
  g_free(memory);
  if (!output) fail("write output");
  vips_shutdown();
}
