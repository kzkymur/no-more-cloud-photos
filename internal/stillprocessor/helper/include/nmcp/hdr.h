// Project-owned source. See LICENSE.md in this directory.
#pragma once

#include <array>
#include <cstdint>

namespace nmcp {

enum class HdrTransfer { pq, hlg };

// BT.2446 Method A policy: BT.2020 RGB, 1000-nit reference HDR display,
// 100-nit BT.709/sRGB output. HLG uses the BT.2100 1000-nit system gamma 1.2.
std::array<std::uint8_t, 3> tone_map_bt2446a(double red, double green, double blue,
                                             HdrTransfer transfer);

// Achromatic reference hook used only to verify the normative Method A curve.
double bt2446a_achromatic_srgb(double input_nits);

}  // namespace nmcp
