# Animation fixture provenance

All animation fixtures used by CI are generated from project-owned source at
test time; no opaque binary fixture is stored in the repository.

- `helper/tests/fixture_generator.cpp` creates a 6x2 three-frame WebP with
  partial frames, blend, background disposal, alpha, durations 40/0/250 ms,
  and four total plays. The zero duration must normalize to 100 ms.
- `helper/tests/gif_fixture_generator.cpp` creates 3x1 GIFs that differ only in
  middle-frame disposal-to-background versus restore-to-previous behavior.
  Their positive centisecond delays and zero-delay normalization are asserted.
- `processor_integration_linux_test.go` embeds the byte-level source of a
  project-owned 1x1 transparent GIF and derives finite/infinite/absent and
  unrepresentable loop fixtures by inserting the standard NETSCAPE2.0 block.

Successful WebP outputs are independently demuxed and composited by
`helper/tests/webp_reference.cpp`; thumbnail AVIF is independently decoded by
the pinned still-helper AVIF reference program. Source generators and expected
properties are reviewable, deterministic evidence in lieu of downloaded media.
