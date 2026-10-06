# Source and dependency licensing

The Go files implementing `nmcp-video-helper` are project-owned original
source and contain no vendored codec source. They are distributed under the
same terms as the no-more-cloud-photos repository. The repository currently
has no root license grant; this file does not create one.

The helper executes the sibling pinned FFmpeg build. That build dynamically
links FFmpeg (LGPL-2.1-or-later with GPL/nonfree components disabled), SVT-AV1
(BSD-3-Clause-Clear plus the Alliance for Open Media Patent License), zimg
(WTFPL-2.0), and libaom (BSD-2-Clause). FFmpeg's native AAC encoder introduces no extra source
dependency. Production packages must retain all upstream notices and complete
their own copyright and codec-patent review.

Exact direct source versions and hashes are recorded in `toolchain.lock` and
the referenced still-helper lock. The Ubuntu/OCI transitive closure is deferred
to issue #20 and is not claimed reproducible by this file.
