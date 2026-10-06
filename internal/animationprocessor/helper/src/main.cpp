// Project-owned source. See ../LICENSE.md.
#include "nmcp_animation/codec_pipeline.h"

#include <cstdlib>
#include <iostream>
#include <libheif/heif.h>
#include <new>

int main(int argc, char **argv) {
  (void)::setenv("OMP_NUM_THREADS", "1", 1);
  bool heif_initialized = false;
  try {
    if (heif_init(nullptr).code != heif_error_Ok) throw nmcp_animation::Failure("capability_failed");
    heif_initialized = true;
    const auto arguments = nmcp_animation::parse_arguments(argc, argv);
    std::string response;
    switch (arguments.command) {
      case nmcp_animation::Arguments::Command::capabilities:
        response = nmcp_animation::capabilities_json(arguments.capabilities);
        break;
      case nmcp_animation::Arguments::Command::inspect:
        response = nmcp_animation::inspect_json(arguments.inspect);
        break;
      case nmcp_animation::Arguments::Command::transform:
        response = nmcp_animation::transform_json(arguments.transform);
        break;
      case nmcp_animation::Arguments::Command::verify_output:
        response = nmcp_animation::verify_output_json(arguments.verify_output);
        break;
    }
    std::cout << response;
    heif_deinit();
    return 0;
  } catch (const nmcp_animation::Failure &failure) {
    std::cout << nmcp_animation::error_json(failure.code);
  } catch (const std::bad_alloc &) {
    std::cout << nmcp_animation::error_json("resource_limit");
  } catch (...) {
    std::cout << nmcp_animation::error_json("processing_failed");
  }
  if (heif_initialized) heif_deinit();
  return 0;
}
