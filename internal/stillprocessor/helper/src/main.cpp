// Project-owned source. See ../LICENSE.md.
#include "nmcp/core.h"
#include "nmcp/image_pipeline.h"

#include <cstdlib>
#include <iostream>
#include <libheif/heif.h>
#include <vips/vips.h>

int main(int argc, char **argv) {
  (void)::setenv("OMP_NUM_THREADS", "1", 1);
  (void)::setenv("OPENBLAS_NUM_THREADS", "1", 1);
  bool heif_initialized = false;
  try {
    if (heif_init(nullptr).code != heif_error_Ok) throw nmcp::Failure("capability_failed");
    heif_initialized = true;
    if (vips_init(argv[0]) != 0) throw nmcp::Failure("capability_failed");
    vips_concurrency_set(1);
    vips_cache_set_max(0);
    const auto arguments = nmcp::parse_arguments(argc, argv);
    const std::string response = arguments.command == nmcp::Arguments::Command::capabilities
                                     ? nmcp::capabilities_json(arguments.capabilities)
                                     : nmcp::transform_json(arguments.transform);
    std::cout << response;
    vips_shutdown();
    if (heif_initialized) heif_deinit();
    return 0;
  } catch (const nmcp::Failure &failure) {
    std::cout << nmcp::error_json(failure.code);
  } catch (...) {
    std::cout << nmcp::error_json("processing_failed");
  }
  vips_shutdown();
  if (heif_initialized) heif_deinit();
  return 0;
}
