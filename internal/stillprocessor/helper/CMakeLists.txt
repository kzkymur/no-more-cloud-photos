cmake_minimum_required(VERSION 3.20)
project(nmcp_still_helper VERSION 1.0.0 LANGUAGES CXX)

include(CTest)

add_library(nmcp_still_core STATIC src/core.cpp src/hdr.cpp)
target_include_directories(nmcp_still_core PUBLIC include)
target_compile_features(nmcp_still_core PUBLIC cxx_std_20)
target_compile_options(nmcp_still_core PRIVATE -Wall -Wextra -Wpedantic -Wconversion -Wshadow)

if(BUILD_TESTING)
  add_executable(nmcp-still-core-tests tests/core_test.cpp)
  target_link_libraries(nmcp-still-core-tests PRIVATE nmcp_still_core)
  add_test(NAME nmcp-still-core-tests COMMAND nmcp-still-core-tests)
endif()

option(NMCP_BUILD_HELPER "Build the dynamically linked production helper" ON)
if(NMCP_BUILD_HELPER)
  find_package(PkgConfig REQUIRED)
  pkg_check_modules(VIPS REQUIRED IMPORTED_TARGET vips)
  pkg_check_modules(HEIF REQUIRED IMPORTED_TARGET libheif)
  pkg_check_modules(LIBRAW REQUIRED IMPORTED_TARGET libraw)
  pkg_check_modules(AOM REQUIRED IMPORTED_TARGET aom)
  pkg_check_modules(LCMS2 REQUIRED IMPORTED_TARGET lcms2)

  add_executable(nmcp-still-helper src/main.cpp src/image_pipeline.cpp)
  target_compile_features(nmcp-still-helper PRIVATE cxx_std_20)
  target_compile_options(nmcp-still-helper PRIVATE -Wall -Wextra -Wpedantic -Wconversion -Wshadow)
  target_link_libraries(nmcp-still-helper PRIVATE
    nmcp_still_core
    PkgConfig::VIPS
    PkgConfig::HEIF
    PkgConfig::LIBRAW
    PkgConfig::AOM
    PkgConfig::LCMS2)
  set_target_properties(nmcp-still-helper PROPERTIES
    RUNTIME_OUTPUT_DIRECTORY "${CMAKE_BINARY_DIR}/bin")

  if(BUILD_TESTING)
    add_executable(nmcp-heif-fixture-generator tests/heif_fixture_generator.cpp)
    target_compile_features(nmcp-heif-fixture-generator PRIVATE cxx_std_20)
    target_compile_options(nmcp-heif-fixture-generator PRIVATE -Wall -Wextra -Wpedantic -Wconversion -Wshadow)
    target_link_libraries(nmcp-heif-fixture-generator PRIVATE PkgConfig::HEIF PkgConfig::LCMS2)
    set_target_properties(nmcp-heif-fixture-generator PROPERTIES
      RUNTIME_OUTPUT_DIRECTORY "${CMAKE_BINARY_DIR}/bin")

    add_executable(nmcp-raw-reference tests/raw_reference.cpp)
    target_compile_features(nmcp-raw-reference PRIVATE cxx_std_20)
    target_compile_options(nmcp-raw-reference PRIVATE -Wall -Wextra -Wpedantic -Wconversion -Wshadow)
    target_link_libraries(nmcp-raw-reference PRIVATE PkgConfig::LIBRAW PkgConfig::VIPS)
    set_target_properties(nmcp-raw-reference PROPERTIES
      RUNTIME_OUTPUT_DIRECTORY "${CMAKE_BINARY_DIR}/bin")
  endif()
endif()
