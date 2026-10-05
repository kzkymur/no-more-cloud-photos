# Source and dependency licensing

The C++ files in this directory are project-owned original source and contain
no vendored codec or color-management source. They are distributed under the
same license terms as the no-more-cloud-photos repository. The repository does
not currently include a root license grant; this file does not create one.

The executable dynamically links libvips, libheif, LibRaw, libaom, and lcms2.
Their licenses and required notices remain those supplied by the pinned system
packages used to build and distribute the executable. A production package must
include those notices and perform its own license-compliance review.
