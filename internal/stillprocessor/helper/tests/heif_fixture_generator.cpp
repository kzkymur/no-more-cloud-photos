// Project-owned source. See ../LICENSE.md.
#include <libheif/heif.h>
#include <lcms2.h>

#include <array>
#include <cstdint>
#include <cstdlib>
#include <filesystem>
#include <fstream>
#include <iostream>
#include <iterator>
#include <memory>
#include <string>
#include <vector>

namespace {

void check(heif_error error) {
  if (error.code != heif_error_Ok) {
    std::cerr << (error.message ? error.message : "libheif error") << '\n';
    std::exit(1);
  }
}

std::vector<std::uint8_t> read_file(const char *path) {
  std::ifstream input(path, std::ios::binary);
  if (!input) std::exit(1);
  return {std::istreambuf_iterator<char>(input), std::istreambuf_iterator<char>()};
}

std::vector<std::uint8_t> display_p3_icc() {
  std::unique_ptr<std::remove_pointer_t<cmsHPROFILE>, decltype(&cmsCloseProfile)> srgb(
      cmsCreate_sRGBProfile(), cmsCloseProfile);
  if (!srgb) std::exit(1);
  cmsCIExyY white{0.3127, 0.3290, 1.0};
  cmsCIExyYTRIPLE primaries{{0.680, 0.320, 1.0}, {0.265, 0.690, 1.0}, {0.150, 0.060, 1.0}};
  cmsToneCurve *curves[3] = {
      static_cast<cmsToneCurve *>(cmsReadTag(srgb.get(), cmsSigRedTRCTag)),
      static_cast<cmsToneCurve *>(cmsReadTag(srgb.get(), cmsSigGreenTRCTag)),
      static_cast<cmsToneCurve *>(cmsReadTag(srgb.get(), cmsSigBlueTRCTag))};
  if (!curves[0] || !curves[1] || !curves[2]) std::exit(1);
  std::unique_ptr<std::remove_pointer_t<cmsHPROFILE>, decltype(&cmsCloseProfile)> profile(
      cmsCreateRGBProfile(&white, &primaries, curves), cmsCloseProfile);
  if (!profile) std::exit(1);
  cmsUInt32Number size = 0;
  if (!cmsSaveProfileToMem(profile.get(), nullptr, &size) || size == 0) std::exit(1);
  std::vector<std::uint8_t> result(size);
  if (!cmsSaveProfileToMem(profile.get(), result.data(), &size)) std::exit(1);
  result.resize(size);
  return result;
}

void encode(const std::filesystem::path &path, heif_transfer_characteristics transfer,
            const std::vector<std::uint8_t> &icc,
            std::array<std::uint8_t, 3> sdr_rgb = {64, 128, 192}, bool display_p3 = false) {
  constexpr int size = 32;
  const bool hdr = transfer == heif_transfer_characteristic_ITU_R_BT_2100_0_PQ ||
                   transfer == heif_transfer_characteristic_ITU_R_BT_2100_0_HLG;
  std::unique_ptr<heif_context, decltype(&heif_context_free)> context(heif_context_alloc(), heif_context_free);
  heif_image *raw_image = nullptr;
  const auto chroma = hdr ? heif_chroma_interleaved_RRGGBB_BE : heif_chroma_interleaved_RGB;
  check(heif_image_create(size, size, heif_colorspace_RGB, chroma, &raw_image));
  std::unique_ptr<heif_image, decltype(&heif_image_release)> image(raw_image, heif_image_release);
  check(heif_image_add_plane(image.get(), heif_channel_interleaved, size, size, hdr ? 10 : 8));
  int stride = 0;
  auto *pixels = heif_image_get_plane(image.get(), heif_channel_interleaved, &stride);
  if (!pixels || stride <= 0) std::exit(1);
  for (int y = 0; y < size; ++y) {
    auto *row = pixels + static_cast<std::size_t>(y) * static_cast<unsigned>(stride);
    for (int x = 0; x < size; ++x) {
      if (hdr) {
        for (int channel = 0; channel < 3; ++channel) {
          const auto offset = static_cast<std::size_t>(x * 6 + channel * 2);
          row[offset] = 0x02;
          row[offset + 1] = 0x00;  // code value 512 / 1023
        }
      } else {
        const auto offset = static_cast<std::size_t>(x * 3);
        row[offset] = sdr_rgb[0];
        row[offset + 1] = sdr_rgb[1];
        row[offset + 2] = sdr_rgb[2];
      }
    }
  }

  std::unique_ptr<heif_color_profile_nclx, decltype(&heif_nclx_color_profile_free)> nclx(
      heif_nclx_color_profile_alloc(), heif_nclx_color_profile_free);
  nclx->color_primaries = hdr ? heif_color_primaries_ITU_R_BT_2020_2_and_2100_0 :
      (display_p3 ? heif_color_primaries_SMPTE_EG_432_1 : heif_color_primaries_ITU_R_BT_709_5);
  nclx->transfer_characteristics = transfer;
  nclx->matrix_coefficients = hdr ? heif_matrix_coefficients_ITU_R_BT_2020_2_non_constant_luminance
                                  : heif_matrix_coefficients_RGB_GBR;
  nclx->full_range_flag = 1;
  check(heif_image_set_nclx_color_profile(image.get(), nclx.get()));
  if (!icc.empty()) check(heif_image_set_raw_color_profile(image.get(), "prof", icc.data(), icc.size()));

  const heif_encoder_descriptor *descriptors[4]{};
  if (heif_get_encoder_descriptors(heif_compression_AV1, "aom", descriptors, 4) <= 0) std::exit(1);
  heif_encoder *raw_encoder = nullptr;
  check(heif_context_get_encoder(context.get(), descriptors[0], &raw_encoder));
  std::unique_ptr<heif_encoder, decltype(&heif_encoder_release)> encoder(raw_encoder, heif_encoder_release);
  check(heif_encoder_set_lossless(encoder.get(), 1));
  check(heif_encoder_set_parameter_integer(encoder.get(), "threads", 1));
  check(heif_encoder_set_parameter_string(encoder.get(), "chroma", "444"));
  std::unique_ptr<heif_encoding_options, decltype(&heif_encoding_options_free)> options(
      heif_encoding_options_alloc(), heif_encoding_options_free);
  if (!options) std::exit(1);
  options->output_nclx_profile = nclx.get();
  options->save_two_colr_boxes_when_ICC_and_nclx_available = icc.empty() ? 0 : 1;
  heif_image_handle *raw_handle = nullptr;
  check(heif_context_encode_image(context.get(), image.get(), encoder.get(), options.get(), &raw_handle));
  std::unique_ptr<heif_image_handle, decltype(&heif_image_handle_release)> handle(raw_handle, heif_image_handle_release);
  check(heif_context_write_to_file(context.get(), path.c_str()));
}

}  // namespace

int main(int argc, char **argv) {
  if (argc != 3) return 2;
  const std::filesystem::path directory(argv[1]);
  std::filesystem::create_directories(directory);
  const auto srgb_icc = read_file(argv[2]);
  const auto p3_icc = display_p3_icc();
  encode(directory / "pq.avif", heif_transfer_characteristic_ITU_R_BT_2100_0_PQ, {});
  encode(directory / "hlg.avif", heif_transfer_characteristic_ITU_R_BT_2100_0_HLG, {});
  encode(directory / "icc-nclx.avif", heif_transfer_characteristic_IEC_61966_2_1, p3_icc,
         {200, 100, 50}, true);
  encode(directory / "invalid-icc.avif", heif_transfer_characteristic_IEC_61966_2_1,
         {'n', 'o', 't', '-', 'a', 'n', '-', 'i', 'c', 'c'});

  // Keep the command-line sRGB profile in the fixture generator's input
  // contract and reject accidental empty/wrong-file invocations.
  if (srgb_icc.size() < 128) return 1;
}
