package r2resolve

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/dataplane"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/r2keys"
)

type publishedNeedStore struct {
	keys      []string
	listing   []r2.ObjectInfo
	lookupErr error
}

func (s publishedNeedStore) ObjectExists(_ context.Context, key string) (bool, error) {
	if s.lookupErr != nil {
		return false, s.lookupErr
	}
	for _, candidate := range s.keys {
		if candidate == key {
			return true, nil
		}
	}
	return false, nil
}

func (s publishedNeedStore) ListObjects(_ context.Context, prefix string) ([]r2.ObjectInfo, error) {
	if s.lookupErr != nil {
		return nil, s.lookupErr
	}
	if s.listing != nil {
		return s.listing, nil
	}
	var objects []r2.ObjectInfo
	for _, key := range s.keys {
		if strings.HasPrefix(key, prefix) {
			objects = append(objects, r2.ObjectInfo{Key: key})
		}
	}
	return objects, nil
}

func TestResolvePublishedNeedDirectoryPreservesNestedPathsAndExcludesSiblings(t *testing.T) {
	key := r2keys.JobAttemptOutputsPrefix(41, 7) + "output/model"
	store := publishedNeedStore{keys: []string{
		key + "/nested/config.json", key + "-backup/leak", key + ".pt", key + "/weights.bin",
	}}
	got, err := ResolvePublishedNeed(context.Background(), store, 41, 7, "output/model:41", "output/model", key)
	if err != nil {
		t.Fatal(err)
	}
	want := []dataplane.ArtifactNeed{
		{Spec: "output/model:41", Path: "output/model/nested/config.json", R2Key: key + "/nested/config.json"},
		{Spec: "output/model:41", Path: "output/model/weights.bin", R2Key: key + "/weights.bin"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resolved = %+v, want %+v", got, want)
	}
}

func TestResolvePublishedNeedRejectsMalformedDirectoryListings(t *testing.T) {
	key := r2keys.JobAttemptOutputsPrefix(41, 7) + "output/model"
	for name, members := range map[string][]string{
		"sibling":                  {key + "-backup/file"},
		"traversal":                {key + "/../escape"},
		"absolute":                 {key + "//escape"},
		"normalized-alias":         {key + "/nested/../file"},
		"backslash":                {key + `/nested\file`},
		"duplicate":                {key + "/file", key + "/file"},
		"file-directory-collision": {key + "/file", key + "/file/child"},
		"directory-marker":         {key + "/"},
	} {
		t.Run(name, func(t *testing.T) {
			store := publishedNeedStore{listing: make([]r2.ObjectInfo, len(members))}
			for i, member := range members {
				store.listing[i].Key = member
			}
			needs, err := ResolvePublishedNeed(context.Background(), store, 41, 7, "output/model:41", "output/model", key)
			if err == nil || len(needs) != 0 || errors.Is(err, ErrArtifactMissing) {
				t.Fatalf("unsafe listing resolved as %+v, %v", needs, err)
			}
		})
	}
}

func TestResolvePublishedNeedKeepsAttemptAndDestinationBoundaries(t *testing.T) {
	key := r2keys.JobAttemptOutputsPrefix(41, 7) + "output/model"
	for _, tc := range []struct{ name, destination, payload string }{
		{"historical-attempt", "output/model", r2keys.JobAttemptOutputsPrefix(41, 6) + "output/model"},
		{"escaping-key", "output/model", key + "/../../escape"},
		{"escaping-destination", "../escape", key},
		{"escaping-prefixed-destination", "/../escape", key},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ResolvePublishedNeed(context.Background(), publishedNeedStore{keys: []string{tc.payload}}, 41, 7, "output/model:41", tc.destination, tc.payload)
			if err == nil || errors.Is(err, ErrArtifactMissing) {
				t.Fatalf("invalid request error = %v", err)
			}
		})
	}
	stale := publishedNeedStore{keys: []string{r2keys.JobAttemptOutputsPrefix(41, 6) + "output/model/file"}}
	if _, err := ResolvePublishedNeed(context.Background(), stale, 41, 7, "output/model:41", "output/model", ""); !errors.Is(err, ErrArtifactMissing) {
		t.Fatalf("historical bytes satisfied current attempt: %v", err)
	}
}

func TestResolvePublishedNeedDistinguishesMissingFromLookupFailure(t *testing.T) {
	key := r2keys.JobAttemptOutputsPrefix(41, 7) + "output/model"
	if _, err := ResolvePublishedNeed(context.Background(), publishedNeedStore{}, 41, 7, "output/model:41", "output/model", key); !errors.Is(err, ErrArtifactMissing) {
		t.Fatalf("empty successful lookup = %v, want missing", err)
	}
	lookupErr := errors.New("network unavailable")
	if _, err := ResolvePublishedNeed(context.Background(), publishedNeedStore{lookupErr: lookupErr}, 41, 7, "output/model:41", "output/model", key); !errors.Is(err, lookupErr) || errors.Is(err, ErrArtifactMissing) {
		t.Fatalf("failed lookup = %v, want unknown rather than missing", err)
	}
}
