// Project-owned source. See ../../LICENSE.md.
#pragma once

#include <cstddef>
#include <cstdint>
#include <span>
#include <stdexcept>
#include <string>
#include <string_view>
#include <utility>
#include <vector>

namespace nmcp_animation {

inline constexpr int kProtocolVersion = 1;
inline constexpr std::string_view kHelperVersion = "nmcp-animation-helper-1.0.0";
inline constexpr int kAbsoluteMaxFrames = 1000;
inline constexpr std::int64_t kAbsoluteMaxDurationMs = 3'600'000;
inline constexpr int kAbsoluteMaxDimension = 100'000;
inline constexpr std::uint64_t kAbsoluteMaxPixels = 1'000'000'000;
inline constexpr std::uint64_t kAbsoluteMaxOutputBytes = 256ULL << 20U;

struct Failure final : std::runtime_error {
  explicit Failure(std::string code);
  std::string code;
};

struct Limits {
  int frames{};
  std::int64_t duration_ms{};
  int dimension{};
  std::uint64_t canvas_pixels{};
  std::uint64_t decoded_pixels{};
  std::uint64_t output_bytes{};
};

struct CapabilityArgs {
  int threads{};
  std::string srgb_icc;
};

struct InspectArgs {
  std::string input;
  std::string input_mime;
  Limits limits;
};

struct TransformArgs {
  std::string input;
  std::string output;
  std::string input_mime;
  std::string output_kind;
  int max_long_edge{};
  int quality{};
  int bit_depth{};
  int threads{};
  std::string srgb_icc;
  Limits limits;
};

struct VerifyOutputArgs {
  std::string input;
  std::string input_mime;
  std::uint64_t maximum_bytes{};
};

struct Arguments {
  enum class Command { capabilities, inspect, transform, verify_output } command;
  CapabilityArgs capabilities;
  InspectArgs inspect;
  TransformArgs transform;
  VerifyOutputArgs verify_output;
};

struct Inspection {
  std::string classification;
  int width{};
  int height{};
  std::vector<int> frame_durations_ms;
  std::int64_t duration_ms{};
  int total_plays{};
  bool has_alpha{};
  std::vector<int> zero_duration_frame_indices;
  std::uint64_t decoded_pixels{};
};

struct FrameInfo {
  int x{};
  int y{};
  int width{};
  int height{};
  int duration_ms{};
  bool has_alpha{};
};

struct Image {
  int width{};
  int height{};
  std::vector<std::uint8_t> rgba;
};

Arguments parse_arguments(int argc, char **argv);
Inspection make_inspection(std::string classification, int width, int height,
                           std::span<const FrameInfo> frames, int total_plays,
                           const Limits &limits);
int gif_total_plays(bool loop_extension_present, unsigned repetition_count);
std::pair<int, int> resize_dimensions(int width, int height, int max_long_edge);
Image resize_premultiplied(const Image &source, int width, int height);
std::string sha256_hex(std::span<const std::byte> input);
std::string json_string(std::string_view input);
std::string error_json(std::string_view code);
std::string inspection_json(const Inspection &inspection);
std::vector<std::byte> read_regular_file(const std::string &path, std::size_t maximum);
void write_all_bounded(const std::string &path, std::span<const std::uint8_t> bytes,
                       std::uint64_t maximum);

}  // namespace nmcp_animation
