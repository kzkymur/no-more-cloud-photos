// Generates a deterministic partial-frame WebP with variable timing, alpha, and four total plays.
#include <array>
#include <algorithm>
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <webp/encode.h>
#include <webp/mux.h>

int main(int argc, char **argv) {
  if (argc != 2) return 2;
  WebPMux *mux = WebPMuxNew();
  if (!mux) return 1;
  WebPMuxAnimParams animation{.bgcolor = 0, .loop_count = 4};
  if (WebPMuxSetAnimationParams(mux, &animation) != WEBP_MUX_OK) return 1;
  const std::array<int, 3> durations = {40, 100, 250};
  const std::array<std::array<std::uint8_t, 4>, 3> colors = {{{255, 0, 0, 255}, {0, 255, 0, 128}, {0, 0, 255, 255}}};
  for (std::size_t index = 0; index < colors.size(); ++index) {
    std::array<std::uint8_t, 16> rgba{};
    for (std::size_t pixel = 0; pixel < 4; ++pixel) std::copy(colors[index].begin(), colors[index].end(), rgba.begin() + pixel * 4);
    std::uint8_t *encoded = nullptr;
    const std::size_t size = WebPEncodeLosslessRGBA(rgba.data(), 2, 2, 8, &encoded);
    if (size == 0) return 1;
    WebPMuxFrameInfo frame{};
    frame.bitstream = {encoded, size}; frame.x_offset = static_cast<int>(index) * 2; frame.y_offset = 0;
    frame.duration = durations[index]; frame.id = WEBP_CHUNK_ANMF;
    frame.dispose_method = index == 1 ? WEBP_MUX_DISPOSE_BACKGROUND : WEBP_MUX_DISPOSE_NONE;
    frame.blend_method = index == 0 ? WEBP_MUX_NO_BLEND : WEBP_MUX_BLEND;
    const WebPMuxError error = WebPMuxPushFrame(mux, &frame, 1);
    WebPFree(encoded);
    if (error != WEBP_MUX_OK) return 1;
  }
  WebPData output{};
  WebPDataInit(&output);
  if (WebPMuxAssemble(mux, &output) != WEBP_MUX_OK) return 1;
  WebPMuxDelete(mux);
  FILE *file = std::fopen(argv[1], "wb");
  if (!file || std::fwrite(output.bytes, 1, output.size, file) != output.size || std::fclose(file) != 0) return 1;
  WebPDataClear(&output);
  return 0;
}
