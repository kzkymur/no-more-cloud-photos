// Independent output probe using libwebp demux metadata and composited decoder timestamps.
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <iostream>
#include <vector>
#include <webp/demux.h>

int main(int argc, char **argv) {
  if (argc != 2) return 2;
  FILE *file = std::fopen(argv[1], "rb");
  if (!file || std::fseek(file, 0, SEEK_END) != 0) return 1;
  const long size = std::ftell(file);
  if (size <= 0 || std::fseek(file, 0, SEEK_SET) != 0) return 1;
  std::vector<std::uint8_t> bytes(static_cast<std::size_t>(size));
  if (std::fread(bytes.data(), 1, bytes.size(), file) != bytes.size() || std::fclose(file) != 0) return 1;
  WebPData data{bytes.data(), bytes.size()};
  WebPDemuxer *demux = WebPDemux(&data);
  if (!demux) return 1;
  const std::uint32_t count = WebPDemuxGetI(demux, WEBP_FF_FRAME_COUNT);
  const std::uint32_t loop = WebPDemuxGetI(demux, WEBP_FF_LOOP_COUNT);
  const int width = static_cast<int>(WebPDemuxGetI(demux, WEBP_FF_CANVAS_WIDTH));
  const int height = static_cast<int>(WebPDemuxGetI(demux, WEBP_FF_CANVAS_HEIGHT));
  std::cout << "{\"width\":" << width
            << ",\"height\":" << height
            << ",\"frames\":" << count << ",\"total_plays\":" << loop << ",\"durations_ms\":[";
  WebPIterator frame{};
  if (!WebPDemuxGetFrame(demux, 1, &frame)) return 1;
  bool first = true;
  do {
    if (!first) std::cout << ',';
    first = false;
    std::cout << frame.duration;
  } while (WebPDemuxNextFrame(&frame));
  WebPDemuxReleaseIterator(&frame);
  WebPDemuxDelete(demux);
  WebPAnimDecoderOptions options{};
  if (!WebPAnimDecoderOptionsInit(&options)) return 1;
  options.color_mode = MODE_RGBA;
  options.use_threads = 0;
  WebPAnimDecoder *decoder = WebPAnimDecoderNew(&data, &options);
  if (!decoder) return 1;
  std::cout << "],\"transparent_pixels\":[";
  first = true;
  std::uint8_t *rgba = nullptr;
  int timestamp = 0;
  std::uint32_t decoded = 0;
  while (WebPAnimDecoderHasMoreFrames(decoder)) {
    if (!WebPAnimDecoderGetNext(decoder, &rgba, &timestamp) || !rgba) return 1;
    std::uint64_t transparent = 0;
    for (std::size_t offset = 3; offset < static_cast<std::size_t>(width) * height * 4U; offset += 4) {
      if (rgba[offset] != 255) ++transparent;
    }
    if (!first) std::cout << ',';
    first = false;
    std::cout << transparent;
    ++decoded;
  }
  WebPAnimDecoderDelete(decoder);
  if (decoded != count) return 1;
  std::cout << "]}";
  return 0;
}
