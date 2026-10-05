// Project-owned source. See ../LICENSE.md.
#include "nmcp/hdr.h"

#include <algorithm>
#include <cmath>

namespace nmcp {
namespace {

constexpr double kHdrPeakNits = 1000.0;
constexpr double kSdrPeakNits = 100.0;
constexpr double kGamma = 2.4;

double clamp(double value) { return std::clamp(value, 0.0, 1.0); }

double pq_eotf_nits(double value) {
  // ITU-R BT.2100 / SMPTE ST 2084 inverse EOTF.
  constexpr double m1 = 2610.0 / 16384.0;
  constexpr double m2 = 2523.0 / 32.0;
  constexpr double c1 = 3424.0 / 4096.0;
  constexpr double c2 = 2413.0 / 128.0;
  constexpr double c3 = 2392.0 / 128.0;
  const double power = std::pow(clamp(value), 1.0 / m2);
  const double denominator = c2 - c3 * power;
  if (denominator <= 0.0) return 10000.0;
  return 10000.0 * std::pow(std::max(power - c1, 0.0) / denominator, 1.0 / m1);
}

double hlg_inverse_oetf(double value) {
  // ITU-R BT.2100 HLG inverse OETF.
  constexpr double a = 0.17883277;
  constexpr double b = 0.28466892;
  constexpr double c = 0.55991073;
  value = clamp(value);
  return value <= 0.5 ? value * value / 3.0 : (std::exp((value - c) / a) + b) / 12.0;
}

double method_a_luma(double hdr_gamma) {
  // ITU-R BT.2446-1 Method A, Tables 2 and 3, 1000 -> 100 cd/m2.
  const double rho_hdr = 1.0 + 32.0 * std::pow(kHdrPeakNits / 10000.0, 1.0 / kGamma);
  const double rho_sdr = 1.0 + 32.0 * std::pow(kSdrPeakNits / 10000.0, 1.0 / kGamma);
  const double perceptual = std::log(1.0 + (rho_hdr - 1.0) * clamp(hdr_gamma)) / std::log(rho_hdr);
  double compressed = 0.0;
  if (perceptual <= 0.7399) compressed = 1.0770 * perceptual;
  else if (perceptual < 0.9909) compressed = -1.1510 * perceptual * perceptual + 2.7811 * perceptual - 0.6302;
  else compressed = 0.5 * perceptual + 0.5;
  return clamp((std::pow(rho_sdr, compressed) - 1.0) / (rho_sdr - 1.0));
}

double linear_to_srgb(double value) {
  value = clamp(value);
  return value <= 0.0031308 ? value * 12.92 : 1.055 * std::pow(value, 1.0 / 2.4) - 0.055;
}

std::uint8_t byte(double value) {
  return static_cast<std::uint8_t>(std::lround(clamp(value) * 255.0));
}

}  // namespace

double bt2446a_achromatic_srgb(double input_nits) {
  const double hdr_gamma = std::pow(clamp(input_nits / kHdrPeakNits), 1.0 / kGamma);
  return linear_to_srgb(std::pow(method_a_luma(hdr_gamma), kGamma));
}

std::array<std::uint8_t, 3> tone_map_bt2446a(double red, double green, double blue,
                                             HdrTransfer transfer) {
  double red_hdr = 0.0;
  double green_hdr = 0.0;
  double blue_hdr = 0.0;
  if (transfer == HdrTransfer::pq) {
    red_hdr = std::pow(clamp(pq_eotf_nits(red) / kHdrPeakNits), 1.0 / kGamma);
    green_hdr = std::pow(clamp(pq_eotf_nits(green) / kHdrPeakNits), 1.0 / kGamma);
    blue_hdr = std::pow(clamp(pq_eotf_nits(blue) / kHdrPeakNits), 1.0 / kGamma);
  } else {
    const double red_scene = hlg_inverse_oetf(red);
    const double green_scene = hlg_inverse_oetf(green);
    const double blue_scene = hlg_inverse_oetf(blue);
    const double scene_luma = 0.2627 * red_scene + 0.6780 * green_scene + 0.0593 * blue_scene;
    // BT.2100 OOTF, gamma=1.2 for a 1000-nit reference display. Values are
    // represented in the 1/2.4 domain required by Method A.
    const double scale = scene_luma > 0.0 ? std::pow(scene_luma, 1.0 / 12.0) : 0.0;
    red_hdr = std::pow(red_scene, 1.0 / kGamma) * scale;
    green_hdr = std::pow(green_scene, 1.0 / kGamma) * scale;
    blue_hdr = std::pow(blue_scene, 1.0 / kGamma) * scale;
  }

  const double hdr_luma = clamp(0.2627 * red_hdr + 0.6780 * green_hdr + 0.0593 * blue_hdr);
  const double sdr_luma = method_a_luma(hdr_luma);
  const double color_scale = hdr_luma > 1.0e-12 ? sdr_luma / (1.1 * hdr_luma) : 0.0;
  const double cb = color_scale * (blue_hdr - hdr_luma) / 1.8814;
  const double cr = color_scale * (red_hdr - hdr_luma) / 1.4746;
  const double adjusted_luma = sdr_luma - std::max(0.1 * cr, 0.0);
  const double red_2020_gamma = clamp(adjusted_luma + 1.4746 * cr);
  const double blue_2020_gamma = clamp(adjusted_luma + 1.8814 * cb);
  const double green_2020_gamma = clamp((adjusted_luma - 0.2627 * red_2020_gamma -
                                         0.0593 * blue_2020_gamma) / 0.6780);

  const double red_2020 = std::pow(red_2020_gamma, kGamma);
  const double green_2020 = std::pow(green_2020_gamma, kGamma);
  const double blue_2020 = std::pow(blue_2020_gamma, kGamma);
  const double red_709 = 1.660491 * red_2020 - 0.587641 * green_2020 - 0.072850 * blue_2020;
  const double green_709 = -0.124550 * red_2020 + 1.132899 * green_2020 - 0.008349 * blue_2020;
  const double blue_709 = -0.018151 * red_2020 - 0.100579 * green_2020 + 1.118730 * blue_2020;
  return {byte(linear_to_srgb(red_709)), byte(linear_to_srgb(green_709)), byte(linear_to_srgb(blue_709))};
}

}  // namespace nmcp
