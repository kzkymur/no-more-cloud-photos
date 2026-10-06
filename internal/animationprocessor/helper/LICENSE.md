# Source and dependency licensing

The C++ files in this directory are project-owned original source and contain
no vendored codec or color-management source. They are distributed under the
same license terms as the no-more-cloud-photos repository. The repository does
not currently include a root license grant; this file does not create one.

The executable dynamically links giflib (MIT), libwebp (BSD-3-Clause), libheif
(LGPL-3.0-or-later), libaom (BSD-2-Clause), and lcms2 (MIT). Production packages
must include the upstream notices and perform their own license-compliance
review. Exact direct source versions and hashes are recorded in this directory's
`toolchain.lock` and the still-helper lock it references.
