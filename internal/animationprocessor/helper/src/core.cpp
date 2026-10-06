// Project-owned source. See ../LICENSE.md.
#include "nmcp_animation/core.h"

#include <algorithm>
#include <array>
#include <cerrno>
#include <charconv>
#include <cmath>
#include <fcntl.h>
#include <iomanip>
#include <limits>
#include <sstream>
#include <sys/stat.h>
#include <unistd.h>

namespace nmcp_animation {
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
  if (index >= argc || std::string_view(argv[index]) != value) throw Failure("policy_violation");
}

Limits parse_limits(int argc, char **argv, int first) {
  static constexpr std::array<std::string_view, 6> flags = {
      "--max-frames", "--max-duration-ms", "--max-dimension", "--max-canvas-pixels",
      "--max-decoded-pixels", "--max-output-bytes"};
  for (std::size_t i = 0; i < flags.size(); ++i) {
    expect(argc, argv, first + static_cast<int>(i * 2U), flags[i]);
  }
  return {
      .frames = parse_number<int>(argv[first + 1], 1, kAbsoluteMaxFrames),
      .duration_ms = parse_number<std::int64_t>(argv[first + 3], 1, kAbsoluteMaxDurationMs),
      .dimension = parse_number<int>(argv[first + 5], 1, kAbsoluteMaxDimension),
      .canvas_pixels = parse_number<std::uint64_t>(argv[first + 7], 1, kAbsoluteMaxPixels),
      .decoded_pixels = parse_number<std::uint64_t>(argv[first + 9], 1, kAbsoluteMaxPixels),
      .output_bytes = parse_number<std::uint64_t>(argv[first + 11], 1, kAbsoluteMaxOutputBytes),
  };
}

std::uint32_t rotr(std::uint32_t value, unsigned count) {
  return (value >> count) | (value << (32U - count));
}

std::string integer_array(std::span<const int> values) {
  std::ostringstream output;
  output << '[';
  for (std::size_t index = 0; index < values.size(); ++index) {
    if (index != 0) output << ',';
    output << values[index];
  }
  output << ']';
  return output.str();
}

int open_regular(const std::string &path, int flags) {
  // Protocol paths are fixed /proc/self/fd descriptors, which are symlinks by design.
  const int descriptor = ::open(path.c_str(), flags | O_CLOEXEC);
  if (descriptor < 0) throw Failure("policy_violation");
  struct stat status {};
  if (::fstat(descriptor, &status) != 0 || !S_ISREG(status.st_mode)) {
    ::close(descriptor);
    throw Failure("policy_violation");
  }
  return descriptor;
}

}  // namespace

Failure::Failure(std::string code_value) : std::runtime_error(code_value), code(std::move(code_value)) {}

Arguments parse_arguments(int argc, char **argv) {
  if (argc == 8 && std::string_view(argv[1]) == "capabilities") {
    expect(argc, argv, 2, "--protocol");
    if (parse_number<int>(argv[3], 1, 1) != kProtocolVersion) throw Failure("policy_violation");
    expect(argc, argv, 4, "--threads");
    const int threads = parse_number<int>(argv[5], 1, 1);
    expect(argc, argv, 6, "--srgb-icc");
    if (std::string_view(argv[7]) != "/proc/self/fd/3") throw Failure("policy_violation");
    Arguments result{};
    result.command = Arguments::Command::capabilities;
    result.capabilities = {.threads = threads, .srgb_icc = argv[7]};
    return result;
  }
  if (argc == 20 && std::string_view(argv[1]) == "inspect") {
    expect(argc, argv, 2, "--protocol");
    if (parse_number<int>(argv[3], 1, 1) != kProtocolVersion) throw Failure("policy_violation");
    expect(argc, argv, 4, "--input");
    expect(argc, argv, 6, "--input-mime");
    const std::string_view mime = argv[7];
    if (std::string_view(argv[5]) != "/proc/self/fd/3" || (mime != "image/gif" && mime != "image/webp")) {
      throw Failure("policy_violation");
    }
    Arguments result{};
    result.command = Arguments::Command::inspect;
    result.inspect = {.input = argv[5], .input_mime = argv[7], .limits = parse_limits(argc, argv, 8)};
    return result;
  }
  if (argc == 34 && std::string_view(argv[1]) == "transform") {
    static constexpr std::array<std::string_view, 10> flags = {
        "--protocol", "--input", "--output", "--input-mime", "--output-kind", "--max-long-edge",
        "--quality", "--bit-depth", "--threads", "--srgb-icc"};
    for (std::size_t i = 0; i < flags.size(); ++i) expect(argc, argv, 2 + static_cast<int>(i * 2U), flags[i]);
    if (parse_number<int>(argv[3], 1, 1) != kProtocolVersion) throw Failure("policy_violation");
    const std::string_view mime = argv[9];
    const std::string_view kind = argv[11];
    if (std::string_view(argv[5]) != "/proc/self/fd/3" || std::string_view(argv[7]) != "/proc/self/fd/4" ||
        std::string_view(argv[21]) != "/proc/self/fd/5" || (mime != "image/gif" && mime != "image/webp") ||
        (kind != "animated-webp" && kind != "first-frame-avif")) {
      throw Failure("policy_violation");
    }
    Arguments result{};
    result.command = Arguments::Command::transform;
    result.transform = {
        .input = argv[5], .output = argv[7], .input_mime = argv[9], .output_kind = argv[11],
        .max_long_edge = parse_number<int>(argv[13], 1, 1920),
        .quality = parse_number<int>(argv[15], 1, 100),
        .bit_depth = parse_number<int>(argv[17], 8, 8),
        .threads = parse_number<int>(argv[19], 1, 1), .srgb_icc = argv[21],
        .limits = parse_limits(argc, argv, 22)};
    return result;
  }
  if (argc == 10 && std::string_view(argv[1]) == "verify-output") {
    expect(argc, argv, 2, "--protocol");
    if (parse_number<int>(argv[3], 1, 1) != kProtocolVersion) throw Failure("policy_violation");
    expect(argc, argv, 4, "--input");
    expect(argc, argv, 6, "--input-mime");
    expect(argc, argv, 8, "--max-output-bytes");
    const std::string_view mime = argv[7];
    if (std::string_view(argv[5]) != "/proc/self/fd/3" || (mime != "image/webp" && mime != "image/avif")) {
      throw Failure("policy_violation");
    }
    Arguments result{};
    result.command = Arguments::Command::verify_output;
    result.verify_output = {.input = argv[5], .input_mime = argv[7],
                            .maximum_bytes = parse_number<std::uint64_t>(argv[9], 1, kAbsoluteMaxOutputBytes)};
    return result;
  }
  throw Failure("policy_violation");
}

Inspection make_inspection(std::string classification, int width, int height,
                           std::span<const FrameInfo> frames, int total_plays,
                           const Limits &limits) {
  if ((classification != "animation" && classification != "static") || width <= 0 || height <= 0 ||
      width > limits.dimension || height > limits.dimension || frames.empty() ||
      frames.size() > static_cast<std::size_t>(limits.frames) || total_plays < 0 || total_plays > 65535) {
    throw Failure("resource_limit");
  }
  const auto canvas = static_cast<std::uint64_t>(width) * static_cast<std::uint64_t>(height);
  if (canvas > limits.canvas_pixels) throw Failure("resource_limit");
  Inspection result{.classification = std::move(classification), .width = width, .height = height,
                    .frame_durations_ms = {}, .duration_ms = 0, .total_plays = total_plays,
                    .has_alpha = false, .zero_duration_frame_indices = {}, .decoded_pixels = 0};
  for (std::size_t index = 0; index < frames.size(); ++index) {
    const auto &frame = frames[index];
    if (frame.x < 0 || frame.y < 0 || frame.width <= 0 || frame.height <= 0 ||
        frame.width > width - frame.x || frame.height > height - frame.y) {
      throw Failure("decode_failed");
    }
    // Both codecs produce a fully composited canvas for every frame. Account
    // that actual decoded surface rather than only a compressed sub-rectangle.
    const auto pixels = canvas;
    if (pixels > limits.decoded_pixels - result.decoded_pixels) throw Failure("resource_limit");
    result.decoded_pixels += pixels;
    int duration = frame.duration_ms;
    if (duration < 0) throw Failure("decode_failed");
    if (duration == 0) {
      duration = 100;
      result.zero_duration_frame_indices.push_back(static_cast<int>(index));
    }
    if (duration > limits.duration_ms || result.duration_ms > limits.duration_ms - duration) {
      throw Failure("resource_limit");
    }
    result.duration_ms += duration;
    result.frame_durations_ms.push_back(duration);
    result.has_alpha = result.has_alpha || frame.has_alpha;
  }
  return result;
}

int gif_total_plays(bool present, unsigned repetitions) {
  if (!present) return 1;
  if (repetitions == 0) return 0;
  if (repetitions >= 65535U) throw Failure("unsupported_input");
  return static_cast<int>(repetitions + 1U);
}

std::pair<int, int> resize_dimensions(int width, int height, int max_long_edge) {
  if (width <= 0 || height <= 0 || max_long_edge <= 0) throw Failure("decode_failed");
  const int longest = std::max(width, height);
  if (longest <= max_long_edge) return {width, height};
  const auto scale = [longest, max_long_edge](int dimension) {
    const auto numerator = static_cast<std::int64_t>(dimension) * max_long_edge;
    return std::max(1, static_cast<int>((numerator + longest / 2) / longest));
  };
  return {scale(width), scale(height)};
}

Image resize_premultiplied(const Image &source, int width, int height) {
  const auto expected = static_cast<std::uint64_t>(source.width) * static_cast<std::uint64_t>(source.height) * 4U;
  if (source.width <= 0 || source.height <= 0 || width <= 0 || height <= 0 ||
      expected != source.rgba.size()) throw Failure("processing_failed");
  if (width == source.width && height == source.height) return source;
  Image output{.width = width, .height = height,
               .rgba = std::vector<std::uint8_t>(static_cast<std::size_t>(width) * static_cast<std::size_t>(height) * 4U)};
  for (int y = 0; y < height; ++y) {
    const double sy = (static_cast<double>(y) + 0.5) * source.height / height - 0.5;
    const int y0 = std::clamp(static_cast<int>(std::floor(sy)), 0, source.height - 1);
    const int y1 = std::min(y0 + 1, source.height - 1);
    const double fy = std::clamp(sy - std::floor(sy), 0.0, 1.0);
    for (int x = 0; x < width; ++x) {
      const double sx = (static_cast<double>(x) + 0.5) * source.width / width - 0.5;
      const int x0 = std::clamp(static_cast<int>(std::floor(sx)), 0, source.width - 1);
      const int x1 = std::min(x0 + 1, source.width - 1);
      const double fx = std::clamp(sx - std::floor(sx), 0.0, 1.0);
      const std::array<double, 4> weights = {(1.0 - fx) * (1.0 - fy), fx * (1.0 - fy),
                                             (1.0 - fx) * fy, fx * fy};
      const std::array<std::pair<int, int>, 4> points = {{{x0, y0}, {x1, y0}, {x0, y1}, {x1, y1}}};
      double alpha = 0.0;
      std::array<double, 3> premul{};
      for (std::size_t sample = 0; sample < points.size(); ++sample) {
        const auto [px, py] = points[sample];
        const std::size_t offset = (static_cast<std::size_t>(py) * static_cast<std::size_t>(source.width) +
                                    static_cast<std::size_t>(px)) * 4U;
        const double a = source.rgba[offset + 3] / 255.0;
        alpha += weights[sample] * a;
        for (std::size_t channel = 0; channel < 3; ++channel) {
          premul[channel] += weights[sample] * a * source.rgba[offset + channel];
        }
      }
      const std::size_t target = (static_cast<std::size_t>(y) * static_cast<std::size_t>(width) +
                                  static_cast<std::size_t>(x)) * 4U;
      output.rgba[target + 3] = static_cast<std::uint8_t>(std::clamp(std::lround(alpha * 255.0), 0L, 255L));
      for (std::size_t channel = 0; channel < 3; ++channel) {
        const long value = alpha == 0.0 ? 0L : std::lround(premul[channel] / alpha);
        output.rgba[target + channel] = static_cast<std::uint8_t>(std::clamp(value, 0L, 255L));
      }
    }
  }
  return output;
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
        if (character < 0x20U) output << "\\u00" << std::hex << std::setw(2) << std::setfill('0') << static_cast<unsigned>(character) << std::dec;
        else output << character;
    }
  }
  output << '"';
  return output.str();
}

std::string error_json(std::string_view code) {
  return "{\"protocol\":1,\"ok\":false,\"error_code\":" + json_string(code) + ",\"result\":null}";
}

std::string inspection_json(const Inspection &i) {
  return "{\"classification\":" + json_string(i.classification) + ",\"width\":" + std::to_string(i.width) +
         ",\"height\":" + std::to_string(i.height) + ",\"frame_count\":" + std::to_string(i.frame_durations_ms.size()) +
         ",\"frame_durations_ms\":" + integer_array(i.frame_durations_ms) + ",\"duration_ms\":" + std::to_string(i.duration_ms) +
         ",\"total_plays\":" + std::to_string(i.total_plays) + ",\"has_alpha\":" + (i.has_alpha ? "true" : "false") +
         ",\"zero_duration_frame_indices\":" + integer_array(i.zero_duration_frame_indices) +
         ",\"decoded_pixels\":" + std::to_string(i.decoded_pixels) + "}";
}

std::vector<std::byte> read_regular_file(const std::string &path, std::size_t maximum) {
  const int descriptor = open_regular(path, O_RDONLY);
  struct stat status {};
  if (::fstat(descriptor, &status) != 0 || status.st_size < 0 || static_cast<std::uint64_t>(status.st_size) > maximum) {
    ::close(descriptor);
    throw Failure("resource_limit");
  }
  std::vector<std::byte> result(static_cast<std::size_t>(status.st_size));
  std::size_t offset = 0;
  while (offset < result.size()) {
    const ssize_t count = ::read(descriptor, result.data() + offset, result.size() - offset);
    if (count < 0 && errno == EINTR) continue;
    if (count <= 0) { ::close(descriptor); throw Failure("decode_failed"); }
    offset += static_cast<std::size_t>(count);
  }
  ::close(descriptor);
  return result;
}

void write_all_bounded(const std::string &path, std::span<const std::uint8_t> bytes, std::uint64_t maximum) {
  if (bytes.empty() || bytes.size() > maximum) throw Failure("output_too_large");
  const int descriptor = open_regular(path, O_WRONLY);
  struct stat status {};
  if (::fstat(descriptor, &status) != 0 || status.st_size != 0 || ::lseek(descriptor, 0, SEEK_CUR) != 0) {
    ::close(descriptor);
    throw Failure("policy_violation");
  }
  std::size_t offset = 0;
  while (offset < bytes.size()) {
    const ssize_t count = ::write(descriptor, bytes.data() + offset, bytes.size() - offset);
    if (count < 0 && errno == EINTR) continue;
    if (count <= 0) { ::close(descriptor); throw Failure(errno == EFBIG ? "output_too_large" : "encode_failed"); }
    offset += static_cast<std::size_t>(count);
  }
  if (::close(descriptor) != 0) throw Failure("encode_failed");
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
    for (std::size_t index = 0; index < 16; ++index) for (unsigned byte = 0; byte < 4; ++byte) words[index] = (words[index] << 8U) | std::to_integer<unsigned>(message[offset + index * 4 + byte]);
    for (std::size_t index = 16; index < words.size(); ++index) {
      const auto x = words[index - 15], y = words[index - 2];
      words[index] = words[index - 16] + (rotr(x,7)^rotr(x,18)^(x>>3U)) + words[index - 7] + (rotr(y,17)^rotr(y,19)^(y>>10U));
    }
    auto [a,b,c,d,e,f,g,h] = state;
    for (std::size_t index = 0; index < words.size(); ++index) {
      const auto temp1 = h + (rotr(e,6)^rotr(e,11)^rotr(e,25)) + ((e&f)^((~e)&g)) + constants[index] + words[index];
      const auto temp2 = (rotr(a,2)^rotr(a,13)^rotr(a,22)) + ((a&b)^(a&c)^(b&c));
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

}  // namespace nmcp_animation
