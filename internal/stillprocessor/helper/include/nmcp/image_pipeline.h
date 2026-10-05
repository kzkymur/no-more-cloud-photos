// Project-owned source. See LICENSE.md in this directory.
#pragma once

#include "nmcp/core.h"

#include <map>
#include <string>
#include <vector>

namespace nmcp {

struct Audit {
  std::string decoder;
  std::string input_color;
  std::string input_primaries;
  std::string input_transfer;
  std::string input_range{"not-applicable"};
  std::string hdr_disposition{"sdr"};
  std::string tone_map{"not-needed"};
  int target_nits{};
  int hdr_peak_nits{};
  int hlg_reference_nits{};
  std::string raw_processing{"not-applicable"};
  std::string alpha;
  std::string chroma;
  int source_width{};
  int source_height{};
};

struct TransformResult {
  int width{};
  int height{};
  Audit audit;
};

std::map<std::string, std::string> library_versions();
std::vector<std::string> decoder_mime_types();
bool aom_encoder_available();
std::vector<std::byte> read_regular_file(const std::string &path, std::size_t maximum);
void validate_srgb_profile(std::span<const std::byte> profile);
TransformResult transform_image(const TransformArgs &args, std::span<const std::byte> srgb_icc);
std::string capabilities_json(const CapabilityArgs &args);
std::string transform_json(const TransformArgs &args);

}  // namespace nmcp
