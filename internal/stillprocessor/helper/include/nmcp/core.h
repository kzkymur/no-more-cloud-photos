// Project-owned source. See LICENSE.md in this directory.
#pragma once

#include <cstddef>
#include <cstdint>
#include <span>
#include <stdexcept>
#include <string>
#include <string_view>
#include <utility>
#include <vector>

namespace nmcp {

inline constexpr int kProtocolVersion = 1;
inline constexpr std::string_view kHelperVersion = "nmcp-still-helper-1.0.0";

struct Failure final : std::runtime_error {
  Failure(std::string code, std::string detail = {});
  std::string code;
};

struct CapabilityArgs {
  std::string srgb_icc;
  int threads{};
};

struct TransformArgs {
  std::string input;
  std::string output;
  std::string input_mime;
  std::string source_mode;
  std::string srgb_icc;
  int max_long_edge{};
  int quality{};
  int bit_depth{};
  int threads{};
  std::uint64_t max_output_bytes{};
};

struct Arguments {
  enum class Command { capabilities, transform } command;
  CapabilityArgs capabilities;
  TransformArgs transform;
};

Arguments parse_arguments(int argc, char **argv);
std::pair<int, int> resize_dimensions(int width, int height, int max_long_edge);
std::string sha256_hex(std::span<const std::byte> input);
std::string json_string(std::string_view input);
std::string error_json(std::string_view code);

// Accepts only Windows BITMAPINFOHEADER-or-later, BI_RGB, 24/32-bit pixels.
int validate_bmp(std::span<const std::byte> header);

}  // namespace nmcp
