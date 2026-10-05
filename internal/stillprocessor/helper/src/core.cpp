// Project-owned source. See ../LICENSE.md.
#include "nmcp/core.h"

#include <array>
#include <charconv>
#include <iomanip>
#include <limits>
#include <sstream>

namespace nmcp {
namespace {

template <typename T>
T parse_number(std::string_view text, T minimum, T maximum) {
  T value{};
  const auto result = std::from_chars(text.data(), text.data() + text.size(), value);
  if (text.empty() || result.ec != std::errc{} || result.ptr != text.data() + text.size() ||
      value < minimum || value > maximum) {
    throw Failure("policy_violation");
  }
  return value;
}

void expect(int argc, char **argv, int index, std::string_view value) {
  if (index >= argc || argv[index] != value) throw Failure("policy_violation");
}

std::uint32_t rotr(std::uint32_t value, unsigned count) {
  return (value >> count) | (value << (32U - count));
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

}  // namespace

Failure::Failure(std::string code_value, std::string detail)
    : std::runtime_error(detail.empty() ? code_value : std::move(detail)), code(std::move(code_value)) {}

Arguments parse_arguments(int argc, char **argv) {
  if (argc >= 2 && std::string_view(argv[1]) == "capabilities") {
    if (argc != 8) throw Failure("policy_violation");
    expect(argc, argv, 2, "--protocol");
    if (parse_number<int>(argv[3], 1, 1) != kProtocolVersion) throw Failure("policy_violation");
    expect(argc, argv, 4, "--threads");
    const int threads = parse_number<int>(argv[5], 1, 1);
    expect(argc, argv, 6, "--srgb-icc");
    if (std::string_view(argv[7]).empty()) throw Failure("policy_violation");
    Arguments result{};
    result.command = Arguments::Command::capabilities;
    result.capabilities = {.srgb_icc = argv[7], .threads = threads};
    return result;
  }
  if (argc >= 2 && std::string_view(argv[1]) == "transform") {
    if (argc != 24) throw Failure("policy_violation");
    static constexpr std::array<std::string_view, 11> flags = {
        "--protocol", "--input", "--output", "--input-mime", "--source-mode", "--max-long-edge",
        "--quality", "--bit-depth", "--threads", "--max-output-bytes", "--srgb-icc"};
    for (std::size_t index = 0; index < flags.size(); ++index) expect(argc, argv, 2 + static_cast<int>(index * 2), flags[index]);
    if (parse_number<int>(argv[3], 1, 1) != kProtocolVersion) throw Failure("policy_violation");
    TransformArgs transform{
        .input = argv[5], .output = argv[7], .input_mime = argv[9], .source_mode = argv[11],
        .srgb_icc = argv[23], .max_long_edge = parse_number<int>(argv[13], 1, 1920),
        .quality = parse_number<int>(argv[15], 1, 100), .bit_depth = parse_number<int>(argv[17], 8, 8),
        .threads = parse_number<int>(argv[19], 1, 1),
        .max_output_bytes = parse_number<std::uint64_t>(argv[21], 1, 64ULL << 20)};
    if (transform.input.empty() || transform.output.empty() || transform.srgb_icc.empty()) throw Failure("policy_violation");
    Arguments result{};
    result.command = Arguments::Command::transform;
    result.transform = std::move(transform);
    return result;
  }
  throw Failure("policy_violation");
}

std::pair<int, int> resize_dimensions(int width, int height, int max_long_edge) {
  if (width <= 0 || height <= 0 || max_long_edge <= 0) throw Failure("decode_failed");
  const int longest = width > height ? width : height;
  if (longest <= max_long_edge) return {width, height};
  const auto scale = [longest, max_long_edge](int dimension) {
    const auto numerator = static_cast<std::int64_t>(dimension) * max_long_edge;
    return std::max(1, static_cast<int>((numerator + longest / 2) / longest));
  };
  return {scale(width), scale(height)};
}

std::string json_string(std::string_view input) {
  std::ostringstream output;
  output << '"';
  for (const unsigned char character : input) {
    switch (character) {
      case '"': output << "\\\""; break;
      case '\\': output << "\\\\"; break;
      case '\b': output << "\\b"; break;
      case '\f': output << "\\f"; break;
      case '\n': output << "\\n"; break;
      case '\r': output << "\\r"; break;
      case '\t': output << "\\t"; break;
      default:
        if (character < 0x20U) {
          output << "\\u00" << std::hex << std::setw(2) << std::setfill('0') << static_cast<unsigned>(character) << std::dec;
        } else {
          output << character;
        }
    }
  }
  output << '"';
  return output.str();
}

std::string error_json(std::string_view code) {
  return "{\"protocol\":1,\"ok\":false,\"error_code\":" + json_string(code) + ",\"result\":null}";
}

int validate_bmp(std::span<const std::byte> bytes) {
  if (bytes.size() < 54 || bytes[0] != std::byte{'B'} || bytes[1] != std::byte{'M'} ||
      little32(bytes, 14) < 40 || little16(bytes, 26) != 1 || little32(bytes, 30) != 0) {
    throw Failure("unsupported_input");
  }
  const int bits = little16(bytes, 28);
  if (bits != 24 && bits != 32) throw Failure("unsupported_input");
  return bits;
}

std::string sha256_hex(std::span<const std::byte> input) {
  static constexpr std::array<std::uint32_t, 64> constants = {
      0x428a2f98,0x71374491,0xb5c0fbcf,0xe9b5dba5,0x3956c25b,0x59f111f1,0x923f82a4,0xab1c5ed5,
      0xd807aa98,0x12835b01,0x243185be,0x550c7dc3,0x72be5d74,0x80deb1fe,0x9bdc06a7,0xc19bf174,
      0xe49b69c1,0xefbe4786,0x0fc19dc6,0x240ca1cc,0x2de92c6f,0x4a7484aa,0x5cb0a9dc,0x76f988da,
      0x983e5152,0xa831c66d,0xb00327c8,0xbf597fc7,0xc6e00bf3,0xd5a79147,0x06ca6351,0x14292967,
      0x27b70a85,0x2e1b2138,0x4d2c6dfc,0x53380d13,0x650a7354,0x766a0abb,0x81c2c92e,0x92722c85,
      0xa2bfe8a1,0xa81a664b,0xc24b8b70,0xc76c51a3,0xd192e819,0xd6990624,0xf40e3585,0x106aa070,
      0x19a4c116,0x1e376c08,0x2748774c,0x34b0bcb5,0x391c0cb3,0x4ed8aa4a,0x5b9cca4f,0x682e6ff3,
      0x748f82ee,0x78a5636f,0x84c87814,0x8cc70208,0x90befffa,0xa4506ceb,0xbef9a3f7,0xc67178f2};
  std::vector<std::byte> message(input.begin(), input.end());
  const std::uint64_t bit_length = static_cast<std::uint64_t>(message.size()) * 8U;
  message.push_back(std::byte{0x80});
  while (message.size() % 64 != 56) message.push_back(std::byte{0});
  for (int shift = 56; shift >= 0; shift -= 8) message.push_back(static_cast<std::byte>((bit_length >> shift) & 0xffU));
  std::array<std::uint32_t, 8> state = {0x6a09e667,0xbb67ae85,0x3c6ef372,0xa54ff53a,0x510e527f,0x9b05688c,0x1f83d9ab,0x5be0cd19};
  for (std::size_t offset = 0; offset < message.size(); offset += 64) {
    std::array<std::uint32_t, 64> words{};
    for (std::size_t index = 0; index < 16; ++index) {
      for (unsigned byte = 0; byte < 4; ++byte) words[index] = (words[index] << 8U) | std::to_integer<unsigned>(message[offset + index * 4 + byte]);
    }
    for (std::size_t index = 16; index < words.size(); ++index) {
      const auto x = words[index - 15], y = words[index - 2];
      words[index] = words[index - 16] + (rotr(x,7)^rotr(x,18)^(x>>3U)) + words[index - 7] + (rotr(y,17)^rotr(y,19)^(y>>10U));
    }
    auto [a,b,c,d,e,f,g,h] = state;
    for (std::size_t index = 0; index < words.size(); ++index) {
      const auto sum1 = rotr(e,6)^rotr(e,11)^rotr(e,25);
      const auto choice = (e&f)^((~e)&g);
      const auto temp1 = h + sum1 + choice + constants[index] + words[index];
      const auto sum0 = rotr(a,2)^rotr(a,13)^rotr(a,22);
      const auto majority = (a&b)^(a&c)^(b&c);
      const auto temp2 = sum0 + majority;
      h=g; g=f; f=e; e=d+temp1; d=c; c=b; b=a; a=temp1+temp2;
    }
    const std::array<std::uint32_t, 8> work = {a,b,c,d,e,f,g,h};
    for (std::size_t index = 0; index < state.size(); ++index) state[index] += work[index];
  }
  std::ostringstream output;
  output << std::hex << std::setfill('0');
  for (const auto value : state) output << std::setw(8) << value;
  return output.str();
}

}  // namespace nmcp
