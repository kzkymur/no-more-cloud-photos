// Project-owned source. See ../LICENSE.md.
#include "nmcp/core.h"

#include <array>
#include <cstdlib>
#include <iostream>
#include <string>
#include <vector>

namespace {

void require(bool condition, const char *message) {
  if (!condition) {
    std::cerr << message << '\n';
    std::exit(1);
  }
}

template <typename Function>
void require_failure(Function function, std::string_view code) {
  try {
    function();
  } catch (const nmcp::Failure &failure) {
    require(failure.code == code, "wrong failure code");
    return;
  }
  require(false, "expected failure");
}

nmcp::Arguments parse(std::vector<std::string> values) {
  std::vector<char *> pointers;
  for (auto &value : values) pointers.push_back(value.data());
  return nmcp::parse_arguments(static_cast<int>(pointers.size()), pointers.data());
}

}  // namespace

int main() {
  const auto capability = parse({"helper", "capabilities", "--protocol", "1", "--threads", "1", "--srgb-icc", "/fd/3"});
  require(capability.command == nmcp::Arguments::Command::capabilities, "capability command");
  require(capability.capabilities.threads == 1, "capability threads");

  const auto transform = parse({"helper", "transform", "--protocol", "1", "--input", "/fd/3", "--output", "/fd/4",
      "--input-mime", "image/jpeg", "--source-mode", "still", "--max-long-edge", "1920", "--quality", "60",
      "--bit-depth", "8", "--threads", "1", "--max-output-bytes", "67108864", "--srgb-icc", "/fd/5"});
  require(transform.transform.quality == 60 && transform.transform.max_output_bytes == 67108864, "transform values");
  require_failure([] { parse({"helper", "capabilities", "--threads", "1", "--protocol", "1", "--srgb-icc", "x"}); },
                  "policy_violation");
  require_failure([] { parse({"helper", "transform", "--protocol", "1", "--input", "i", "--output", "o",
      "--input-mime", "image/jpeg", "--source-mode", "still", "--max-long-edge", "1921", "--quality", "60",
      "--bit-depth", "8", "--threads", "1", "--max-output-bytes", "1", "--srgb-icc", "p"}); }, "policy_violation");

  require(nmcp::resize_dimensions(3000, 2001, 1920) == std::pair(1920, 1281), "rounded resize");
  require(nmcp::resize_dimensions(1, 9999, 1920) == std::pair(1, 1920), "one-pixel resize");
  require(nmcp::resize_dimensions(101, 99, 1920) == std::pair(101, 99), "no upscale");

  const std::string abc = "abc";
  const auto bytes = std::as_bytes(std::span(abc.data(), abc.size()));
  require(nmcp::sha256_hex(bytes) == "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad", "sha256");
  require(nmcp::json_string("a\n\"b") == "\"a\\n\\\"b\"", "json escaping");
  require(nmcp::error_json("decode_failed") ==
              "{\"protocol\":1,\"ok\":false,\"error_code\":\"decode_failed\",\"result\":null}", "error json");

  std::array<std::byte, 54> bmp{};
  bmp[0] = std::byte{'B'}; bmp[1] = std::byte{'M'};
  bmp[14] = std::byte{40}; bmp[26] = std::byte{1}; bmp[28] = std::byte{24};
  require(nmcp::validate_bmp(bmp) == 24, "BMP 24");
  bmp[28] = std::byte{32};
  require(nmcp::validate_bmp(bmp) == 32, "BMP 32");
  bmp[30] = std::byte{1};
  require_failure([&] { nmcp::validate_bmp(bmp); }, "unsupported_input");
}
