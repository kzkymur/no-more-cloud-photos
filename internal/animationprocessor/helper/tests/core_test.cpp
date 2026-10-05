// Project-owned source. See ../LICENSE.md.
#include "nmcp_animation/core.h"

#include <array>
#include <cstdlib>
#include <iostream>
#include <string>
#include <vector>

namespace {

int failures = 0;

void check(bool condition, const char *message) {
  if (!condition) {
    std::cerr << "FAIL: " << message << '\n';
    ++failures;
  }
}

template <typename Function>
void fails_with(Function function, std::string_view code, const char *message) {
  try {
    function();
    check(false, message);
  } catch (const nmcp_animation::Failure &failure) {
    check(failure.code == code, message);
  }
}

nmcp_animation::Arguments parse(std::vector<std::string> values) {
  std::vector<char *> pointers;
  for (auto &value : values) pointers.push_back(value.data());
  return nmcp_animation::parse_arguments(static_cast<int>(pointers.size()), pointers.data());
}

nmcp_animation::Limits limits() {
  return {.frames = 3, .duration_ms = 1000, .dimension = 100, .canvas_pixels = 10000,
          .decoded_pixels = 1000, .output_bytes = 1024};
}

}  // namespace

int main() {
  using namespace nmcp_animation;
  const auto capability = parse({"helper", "capabilities", "--protocol", "1", "--threads", "1", "--srgb-icc", "/proc/self/fd/3"});
  check(capability.command == Arguments::Command::capabilities && capability.capabilities.threads == 1,
        "capability argument order");

  const auto inspection = parse({"helper", "inspect", "--protocol", "1", "--input", "/proc/self/fd/3",
      "--input-mime", "image/gif", "--max-frames", "3", "--max-duration-ms", "1000",
      "--max-dimension", "100", "--max-canvas-pixels", "10000", "--max-decoded-pixels", "1000",
      "--max-output-bytes", "1024"});
  check(inspection.command == Arguments::Command::inspect && inspection.inspect.limits.frames == 3,
        "inspect exact protocol");

  const auto transform = parse({"helper", "transform", "--protocol", "1", "--input", "/proc/self/fd/3",
      "--output", "/proc/self/fd/4", "--input-mime", "image/webp", "--output-kind", "animated-webp",
      "--max-long-edge", "1920", "--quality", "80", "--bit-depth", "8", "--threads", "1",
      "--srgb-icc", "/proc/self/fd/5", "--max-frames", "3", "--max-duration-ms", "1000",
      "--max-dimension", "100", "--max-canvas-pixels", "10000", "--max-decoded-pixels", "1000",
      "--max-output-bytes", "1024"});
  check(transform.command == Arguments::Command::transform && transform.transform.quality == 80,
        "transform exact protocol");

  fails_with([] { (void)parse({"helper", "capabilities", "--threads", "1", "--protocol", "1", "--srgb-icc", "x"}); },
             "policy_violation", "reject reordered flags");
  fails_with([] { (void)parse({"helper", "capabilities", "--protocol", "1", "--threads", "1", "--srgb-icc", "/tmp/profile"}); },
             "policy_violation", "reject non-protocol descriptor path");
  fails_with([] { (void)parse({"helper", "inspect", "--protocol", "1", "--input", "x", "--input-mime", "image/png",
      "--max-frames", "3", "--max-duration-ms", "1000", "--max-dimension", "100", "--max-canvas-pixels", "10000",
      "--max-decoded-pixels", "1000", "--max-output-bytes", "1024"}); },
      "policy_violation", "reject unsupported MIME");
  fails_with([] { (void)parse({"helper", "inspect", "--protocol", "1", "--input", "x", "--input-mime", "image/gif",
      "--max-frames", "1001", "--max-duration-ms", "1000", "--max-dimension", "100", "--max-canvas-pixels", "10000",
      "--max-decoded-pixels", "1000", "--max-output-bytes", "1024"}); },
      "policy_violation", "reject ceiling above absolute policy");

  const std::array<FrameInfo, 3> frames = {{{0, 0, 3, 2, 40, false}, {1, 0, 2, 2, 0, true}, {0, 0, 3, 2, 250, false}}};
  const Inspection result = make_inspection("animation", 3, 2, frames, 4, limits());
  check(result.duration_ms == 390 && result.frame_durations_ms[1] == 100 &&
        result.zero_duration_frame_indices == std::vector<int>{1} && result.decoded_pixels == 16,
        "duration normalization and source rectangle accounting");
  check(gif_total_plays(false, 99) == 1 && gif_total_plays(true, 0) == 0 && gif_total_plays(true, 3) == 4,
        "GIF total-play normalization");
  fails_with([&] { auto small = limits(); small.decoded_pixels = 15; (void)make_inspection("animation", 3, 2, frames, 4, small); },
             "resource_limit", "decoded source rectangle ceiling");
  fails_with([&] { auto small = limits(); small.duration_ms = 389; (void)make_inspection("animation", 3, 2, frames, 4, small); },
             "resource_limit", "duration ceiling");
  fails_with([&] { auto small = limits(); small.canvas_pixels = 5; (void)make_inspection("animation", 3, 2, frames, 4, small); },
             "resource_limit", "canvas ceiling");

  check(resize_dimensions(3000, 2001, 1920) == std::pair{1920, 1281}, "odd resize rounding");
  check(resize_dimensions(100000, 1, 1920) == std::pair{1920, 1}, "one pixel axis preserved");
  Image alpha{.width = 2, .height = 1, .rgba = {255, 0, 0, 255, 0, 255, 0, 0}};
  const Image resized = resize_premultiplied(alpha, 1, 1);
  check(resized.rgba[0] == 255 && resized.rgba[1] == 0 && resized.rgba[2] == 0 && resized.rgba[3] == 128,
        "premultiplied resize prevents transparent color bleed");

  const std::array<std::byte, 3> abc = {std::byte{'a'}, std::byte{'b'}, std::byte{'c'}};
  check(sha256_hex(abc) == "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad", "SHA-256");
  check(error_json("decode_failed") == "{\"protocol\":1,\"ok\":false,\"error_code\":\"decode_failed\",\"result\":null}",
        "stable error envelope");

  return failures == 0 ? EXIT_SUCCESS : EXIT_FAILURE;
}
