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
  std::cout << "{\"width\":" << WebPDemuxGetI(demux, WEBP_FF_CANVAS_WIDTH)
            << ",\"height\":" << WebPDemuxGetI(demux, WEBP_FF_CANVAS_HEIGHT)
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
  std::cout << "]}";
  return 0;
}
