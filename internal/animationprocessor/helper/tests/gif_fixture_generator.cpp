// Generates deterministic project-owned GIF disposal fixtures.
#include <array>
#include <cstdint>
#include <filesystem>
#include <gif_lib.h>
#include <span>
#include <vector>

namespace {

bool extension(GifFileType *gif, int disposal, int delay, bool transparent) {
  const std::array<GifByteType, 4> block = {
      static_cast<GifByteType>((disposal << 2) | (transparent ? 1 : 0)),
      static_cast<GifByteType>(delay & 0xff), static_cast<GifByteType>((delay >> 8) & 0xff),
      static_cast<GifByteType>(transparent ? 0 : NO_TRANSPARENT_COLOR)};
  return EGifPutExtension(gif, GRAPHICS_EXT_FUNC_CODE, static_cast<int>(block.size()), block.data()) != GIF_ERROR;
}

bool image(GifFileType *gif, int left, int width, std::span<const GifPixelType> pixels) {
  return EGifPutImageDesc(gif, left, 0, width, 1, false, nullptr) != GIF_ERROR &&
         EGifPutLine(gif, const_cast<GifPixelType *>(pixels.data()), width) != GIF_ERROR;
}

bool generate(const std::filesystem::path &path, int middle_disposal) {
  const std::array<GifColorType, 4> colors = {{{0, 0, 0}, {255, 0, 0}, {0, 255, 0}, {0, 0, 255}}};
  ColorMapObject *map = GifMakeMapObject(static_cast<int>(colors.size()), colors.data());
  if (!map) return false;
  int error = 0;
  GifFileType *gif = EGifOpenFileName(path.c_str(), false, &error);
  if (!gif) { GifFreeMapObject(map); return false; }
  const std::array<GifPixelType, 3> red = {1, 1, 1};
  const std::array<GifPixelType, 1> green = {2};
  const std::array<GifPixelType, 1> blue = {3};
  bool ok = EGifPutScreenDesc(gif, 3, 1, 2, 0, map) != GIF_ERROR &&
            extension(gif, DISPOSE_DO_NOT, 4, true) && image(gif, 0, 3, red) &&
            extension(gif, middle_disposal, 0, true) && image(gif, 1, 1, green) &&
            extension(gif, DISPOSE_DO_NOT, 25, true) && image(gif, 2, 1, blue);
  if (EGifCloseFile(gif, &error) == GIF_ERROR) ok = false;
  GifFreeMapObject(map);
  return ok;
}

bool generate_incompressible(const std::filesystem::path &path, int frames) {
  std::array<GifColorType, 256> colors{};
  for (std::size_t index = 0; index < colors.size(); ++index) {
    colors[index] = {static_cast<GifByteType>(index), static_cast<GifByteType>(index * 73U),
                     static_cast<GifByteType>(index * 151U)};
  }
  ColorMapObject *map = GifMakeMapObject(static_cast<int>(colors.size()), colors.data());
  if (!map) return false;
  int error = 0;
  GifFileType *gif = EGifOpenFileName(path.c_str(), false, &error);
  if (!gif) { GifFreeMapObject(map); return false; }
  bool ok = EGifPutScreenDesc(gif, 64, 64, 8, 0, map) != GIF_ERROR;
  std::vector<GifPixelType> row(64);
  std::uint32_t state = 0x7a91bc23U;
  for (int frame = 0; ok && frame < frames; ++frame) {
    ok = extension(gif, DISPOSE_DO_NOT, 1, false) &&
         EGifPutImageDesc(gif, 0, 0, 64, 64, false, nullptr) != GIF_ERROR;
    for (int y = 0; ok && y < 64; ++y) {
      for (auto &pixel : row) {
        state = state * 1664525U + 1013904223U;
        pixel = static_cast<GifPixelType>(state >> 24U);
      }
      ok = EGifPutLine(gif, row.data(), static_cast<int>(row.size())) != GIF_ERROR;
    }
  }
  if (EGifCloseFile(gif, &error) == GIF_ERROR) ok = false;
  GifFreeMapObject(map);
  return ok;
}

}  // namespace

int main(int argc, char **argv) {
  if (argc != 2) return 2;
  const std::filesystem::path directory(argv[1]);
  std::filesystem::create_directories(directory);
  return generate(directory / "dispose-background.gif", DISPOSE_BACKGROUND) &&
                 generate(directory / "dispose-previous.gif", DISPOSE_PREVIOUS) &&
                 generate_incompressible(directory / "incompressible-single.gif", 1) &&
                 generate_incompressible(directory / "incompressible-many.gif", 128) ? 0 : 1;
}
