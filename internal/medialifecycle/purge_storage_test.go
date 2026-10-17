package medialifecycle

import (
	"context"
	"errors"
	"testing"

	"github.com/kzkymur/no-more-cloud-photos/internal/storage"
)

func TestStoragePurgeFileActionRejectsMismatchedTypedKeyIdentity(t *testing.T) {
	store, err := storage.Open(t.TempDir(), storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	action := StoragePurgeFileAction(store)
	tests := []PurgeFile{
		{
			Kind:         PurgeFileOriginal,
			ObjectID:     integrationUUID(901),
			RelativePath: "originals/10/" + integrationUUID(902) + "/original.jpg",
			SizeBytes:    1,
		},
		{
			Kind:         PurgeFileRendition,
			ObjectID:     integrationUUID(903),
			RelativePath: "renditions/10/" + integrationUUID(904) + "/" + integrationUUID(905) + "/" + integrationUUID(906) + ".avif",
			SizeBytes:    1,
		},
	}
	for _, file := range tests {
		if _, err := action(context.Background(), file); !errors.Is(err, storage.ErrValidation) {
			t.Fatalf("kind %s mismatched identity error = %#v", file.Kind, err)
		}
	}
}
