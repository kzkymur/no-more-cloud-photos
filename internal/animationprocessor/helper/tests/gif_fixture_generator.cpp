// Generates deterministic project-owned GIF disposal fixtures.
#include <array>
#include <filesystem>
#include <gif_lib.h>
#include <span>

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

}  // namespace

int main(int argc, char **argv) {
  if (argc != 2) return 2;
  const std::filesystem::path directory(argv[1]);
  std::filesystem::create_directories(directory);
  return generate(directory / "dispose-background.gif", DISPOSE_BACKGROUND) &&
                 generate(directory / "dispose-previous.gif", DISPOSE_PREVIOUS) ? 0 : 1;
}
