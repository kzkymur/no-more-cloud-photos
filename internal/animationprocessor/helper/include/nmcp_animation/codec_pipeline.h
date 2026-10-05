// Project-owned source. See ../../LICENSE.md.
#pragma once

#include "nmcp_animation/core.h"

#include <map>
#include <string>
#include <vector>

namespace nmcp_animation {

struct Animation {
  Inspection inspection;
  std::vector<Image> frames;
  std::vector<std::byte> embedded_icc;
  std::string decoder;
};

std::map<std::string, std::string> library_versions();
std::string capabilities_json(const CapabilityArgs &args);
std::string inspect_json(const InspectArgs &args);
std::string transform_json(const TransformArgs &args);

}  // namespace nmcp_animation
