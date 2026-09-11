package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/minio/minio-go/v7"
)

// The Pi's MinIO user is read-only over the WHOLE `comic-flex` bucket and its
// configured storage.prefix is "", so whatever is in the bucket is what goes on
// the wall. The companion web app is about to start writing user uploads into
// that same bucket under `u/<owner-uuid>/`, which would put strangers' comics on
// a household TV. These tests cover the subtraction that prevents it.
//
// Fixture keys are taken from the real bucket's shape on purpose: the names
// carry spaces and an apostrophe, and "Comics 2022" really is a byte prefix of a
// DIFFERENT top-level directory. A fixture of "a/b.jpg"-style names cannot see
// either hazard.

const (
	keyOwnedComic   = "Comics 2022-2 (jan-aug 2024)/page-01.jpg"
	keyApostrophe   = "Laura's Comics/vol 3/page 12.jpg"
	keyUserUpload   = "u/3f6c1b52-0b6e-4f4a-9a2e-0c2f1d7b8e10/spam.png"
	keyStraddlesU   = "unrelated/z.jpg"
	keyOwnedArchive = "Archive/2019/scan (1).png"
)

// ---------------------------------------------------------------------------
// The predicate
// ---------------------------------------------------------------------------

func TestExcludedMatchesOnlyWholeDirectories(t *testing.T) {
	// Every prefix here is one validateExcludePrefixes would accept, because
	// that is the only kind excluded() is ever handed in production. The
	// slashless case is covered separately, below, where it belongs: at the
	// refusal.
	cases := []struct {
		name     string
		key      string
		prefixes []string
		want     bool
	}{
		{
			name:     "key under a listed directory is excluded",
			key:      keyUserUpload,
			prefixes: []string{"u/"},
			want:     true,
		},
		{
			name:     "key under a different top-level directory is kept",
			key:      keyOwnedComic,
			prefixes: []string{"u/"},
			want:     false,
		},
		{
			// The straddle. "unrelated/z.jpg" starts with the letter u and would
			// be excluded by a "u" rule; the separator is what stops it.
			name:     "a u/ entry does not reach a sibling whose name merely starts with u",
			key:      keyStraddlesU,
			prefixes: []string{"u/"},
			want:     false,
		},
		{
			name:     "empty list keeps everything",
			key:      keyUserUpload,
			prefixes: nil,
			want:     false,
		},
		{
			name:     "empty (non-nil) list keeps everything",
			key:      keyUserUpload,
			prefixes: []string{},
			want:     false,
		},
		{
			name:     "spaces and an apostrophe in the key are matched literally, not as patterns",
			key:      keyApostrophe,
			prefixes: []string{"Laura's Comics/"},
			want:     true,
		},
		{
			// filepath.Match would treat neither of these as a match for the
			// other; a byte prefix does. This pins that we are doing the latter.
			name:     "a deep key is excluded by its top-level directory",
			key:      keyOwnedArchive,
			prefixes: []string{"Archive/"},
			want:     true,
		},
		{
			name:     "the second entry of a list can match",
			key:      keyUserUpload,
			prefixes: []string{"Trash/", "u/"},
			want:     true,
		},
		{
			name:     "no entry matches",
			key:      keyApostrophe,
			prefixes: []string{"Trash/", "u/", "Archive/"},
			want:     false,
		},
		{
			// A prefix that is longer than the key must not match, and must not
			// panic.
			name:     "a prefix longer than the key does not match",
			key:      "u/",
			prefixes: []string{"u/3f6c1b52-0b6e-4f4a-9a2e-0c2f1d7b8e10/"},
			want:     false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := excluded(tc.key, tc.prefixes); got != tc.want {
				t.Fatalf("excluded(%q, %v) = %v, want %v", tc.key, tc.prefixes, got, tc.want)
			}
		})
	}
}

// TestSlashlessPrefixReallyDoesSwallowASibling is the faithful reproduction of
// the hazard the refusal exists for, in the style of this package's other
// pre-fix twins. It is NOT a regression guard on shipped behaviour — the config
// path can no longer produce this input — it is the evidence that the invariant
// is load-bearing. If byte-prefix matching ever stopped behaving this way, the
// refusal below would be pinning nothing and this test says so.
func TestSlashlessPrefixReallyDoesSwallowASibling(t *testing.T) {
	if !excluded(keyOwnedComic, []string{"Comics 2022"}) {
		t.Fatalf("the reproduction is no longer faithful: %q is not matched by the slashless "+
			"prefix %q, so the trailing-slash requirement would be guarding nothing",
			keyOwnedComic, "Comics 2022")
	}
	// And the same directory survives once the entry names a real boundary,
	// which is what the operator meant to write.
	if excluded(keyOwnedComic, []string{"Comics 2022/"}) {
		t.Fatalf("excluded(%q, [%q]) = true; the separator must confine the rule to the "+
			"directory literally named", keyOwnedComic, "Comics 2022/")
	}
}

// ---------------------------------------------------------------------------
// The refusal
// ---------------------------------------------------------------------------

func TestValidateExcludePrefixes(t *testing.T) {
	cases := []struct {
		name      string
		prefixes  []string
		wantErr   bool
		wantInErr []string // substrings the message must carry
	}{
		{name: "nil list is valid", prefixes: nil},
		{name: "empty list is valid", prefixes: []string{}},
		{name: "single directory entry is valid", prefixes: []string{"u/"}},
		{name: "nested directory entry is valid", prefixes: []string{"u/3f6c1b52/"}},
		{name: "entry with spaces and an apostrophe is valid", prefixes: []string{"Laura's Comics/"}},
		{
			name:      "slashless entry is refused",
			prefixes:  []string{"Comics 2022"},
			wantErr:   true,
			wantInErr: []string{"exclude_prefixes[0]", `"Comics 2022"`, "must end with"},
		},
		{
			name:      "a slashless entry is refused even when an earlier entry is fine",
			prefixes:  []string{"u/", "Trash"},
			wantErr:   true,
			wantInErr: []string{"exclude_prefixes[1]", `"Trash"`, "must end with"},
		},
		{
			name:      "empty entry is refused",
			prefixes:  []string{""},
			wantErr:   true,
			wantInErr: []string{"exclude_prefixes[0]", "empty"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateExcludePrefixes(tc.prefixes)
			if tc.wantErr && err == nil {
				t.Fatalf("validateExcludePrefixes(%v) = nil, want an error naming the bad entry", tc.prefixes)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("validateExcludePrefixes(%v) = %v, want nil", tc.prefixes, err)
			}
			for _, want := range tc.wantInErr {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q does not mention %q — an operator has to be able to "+
						"see WHICH entry is wrong", err.Error(), want)
				}
			}
		})
	}
}

// TestNewS3StoreRefusesASlashlessExcludePrefix pins where the refusal lands:
// at store construction, which main() runs before gtk.Init and before any image
// is drawn. Nothing is set up that would have to be torn down.
//
// The credentials are set so that a failure here is unambiguously the exclude
// list and not the environment — the validation deliberately runs before the
// credential lookup, and this test is what holds that ordering.
func TestNewS3StoreRefusesASlashlessExcludePrefix(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "test-access-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret-key")

	store, err := NewS3Store("s3.example.invalid", "comic-flex", "", []string{"Comics 2022"}, true, true)
	if err == nil {
		t.Fatalf("NewS3Store accepted the slashless entry %q and returned a store; a "+
			"slashless entry must be a config error, not a silent literal prefix", "Comics 2022")
	}
	if store != nil {
		t.Fatalf("NewS3Store returned a non-nil store alongside an error: %v", err)
	}
	if !strings.Contains(err.Error(), "must end with") || !strings.Contains(err.Error(), "Comics 2022") {
		t.Fatalf("NewS3Store failed for the wrong reason: %v (want the exclude_prefixes refusal "+
			"naming the entry)", err)
	}
}

func TestNewS3StoreKeepsAValidatedExcludeList(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "test-access-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret-key")

	want := []string{"u/", "Trash/"}
	store, err := NewS3Store("s3.example.invalid", "comic-flex", "", want, true, true)
	if err != nil {
		t.Fatalf("NewS3Store rejected a valid exclude list: %v", err)
	}
	if len(store.excludePrefixes) != len(want) {
		t.Fatalf("store.excludePrefixes = %v, want %v", store.excludePrefixes, want)
	}
	for i := range want {
		if store.excludePrefixes[i] != want[i] {
			t.Fatalf("store.excludePrefixes = %v, want %v", store.excludePrefixes, want)
		}
	}
	// The wiring is only worth anything if the predicate reads it, so assert the
	// end state rather than the field alone.
	if !excluded(keyUserUpload, store.excludePrefixes) {
		t.Fatalf("a store built with %v does not exclude %q", want, keyUserUpload)
	}
	if excluded(keyOwnedComic, store.excludePrefixes) {
		t.Fatalf("a store built with %v excludes the owner's own comic %q", want, keyOwnedComic)
	}
}

// TestNewS3StoreWithNoExcludeListIsTodaysBehaviour: the feature is inert until
// configured. An absent key must not change what reaches the display.
func TestNewS3StoreWithNoExcludeListIsTodaysBehaviour(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "test-access-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret-key")

	store, err := NewS3Store("s3.example.invalid", "comic-flex", "", nil, true, true)
	if err != nil {
		t.Fatalf("NewS3Store rejected an absent exclude list: %v", err)
	}
	for _, key := range []string{keyOwnedComic, keyApostrophe, keyUserUpload, keyStraddlesU, keyOwnedArchive} {
		if excluded(key, store.excludePrefixes) {
			t.Fatalf("with no exclude list configured, %q was excluded; the feature must be "+
				"inert until it is configured", key)
		}
	}
}

// ---------------------------------------------------------------------------
// The seam: the list is applied where the bucket is drained
// ---------------------------------------------------------------------------

// feedObjects returns a closed channel carrying one ObjectInfo per key, which is
// what minio.Client.ListObjects hands ListImages.
func feedObjects(keys ...string) <-chan minio.ObjectInfo {
	ch := make(chan minio.ObjectInfo, len(keys))
	for _, k := range keys {
		ch <- minio.ObjectInfo{Key: k}
	}
	close(ch)
	return ch
}

// TestCollectImageKeysAppliesTheExcludeList covers the seam rather than either
// component: excluded() being correct proves nothing if the drain loop never
// calls it, and every test above passes with the call site deleted.
func TestCollectImageKeysAppliesTheExcludeList(t *testing.T) {
	all := []string{
		keyOwnedComic,
		keyApostrophe,
		keyUserUpload,
		keyStraddlesU,
		"Comics 2022/notes.txt", // right directory, not an image
	}

	t.Run("with an exclude list", func(t *testing.T) {
		s := &S3Store{excludePrefixes: []string{"u/"}}
		got, err := s.collectImageKeys(feedObjects(all...))
		if err != nil {
			t.Fatalf("collectImageKeys: %v", err)
		}
		want := []string{keyOwnedComic, keyApostrophe, keyStraddlesU}
		assertKeys(t, got, want)
	})

	t.Run("with no exclude list nothing is dropped but non-images", func(t *testing.T) {
		s := &S3Store{}
		got, err := s.collectImageKeys(feedObjects(all...))
		if err != nil {
			t.Fatalf("collectImageKeys: %v", err)
		}
		want := []string{keyOwnedComic, keyApostrophe, keyUserUpload, keyStraddlesU}
		assertKeys(t, got, want)
	})

	t.Run("a listing error still propagates", func(t *testing.T) {
		ch := make(chan minio.ObjectInfo, 2)
		ch <- minio.ObjectInfo{Key: keyOwnedComic}
		ch <- minio.ObjectInfo{Err: errListing}
		close(ch)
		s := &S3Store{excludePrefixes: []string{"u/"}}
		got, err := s.collectImageKeys(ch)
		if err == nil {
			t.Fatalf("collectImageKeys swallowed the listing error, returned %v", got)
		}
		if !errors.Is(err, errListing) {
			t.Fatalf("collectImageKeys returned %v, want %v", err, errListing)
		}
	})
}

var errListing = errors.New("listing blew up")

func assertKeys(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("collectImageKeys returned %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("collectImageKeys returned %v, want %v", got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// The YAML key
// ---------------------------------------------------------------------------

// TestLoadConfigParsesExcludePrefixes pins the yaml tag. A struct field with a
// mis-spelled tag parses to nil and excludes nothing — the failure mode is
// silence, which on this feature means the uploads go on the TV anyway.
func TestLoadConfigParsesExcludePrefixes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := "" +
		"storage:\n" +
		"  backend: \"s3\"\n" +
		"  bucket: \"comic-flex\"\n" +
		"  prefix: \"\"\n" +
		"  exclude_prefixes:\n" +
		"    - \"u/\"\n" +
		"    - \"Laura's Comics/\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing fixture config: %v", err)
	}

	config, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	want := []string{"u/", "Laura's Comics/"}
	if len(config.Storage.ExcludePrefixes) != len(want) {
		t.Fatalf("config.Storage.ExcludePrefixes = %v, want %v (is the yaml tag `exclude_prefixes`?)",
			config.Storage.ExcludePrefixes, want)
	}
	for i := range want {
		if config.Storage.ExcludePrefixes[i] != want[i] {
			t.Fatalf("config.Storage.ExcludePrefixes = %v, want %v",
				config.Storage.ExcludePrefixes, want)
		}
	}
}

// TestConfigCarriesAnExcludeListAtAll is the RED WITNESS for this change, and
// the reflection is the whole point of it: every other test in this file names
// symbols that did not exist before the change, so against the pre-change tree
// they do not fail, they do not COMPILE — which proves the tests are new, not
// that they can catch anything. This one compiles against the pre-change
// StorageConfig and fails at run time with the sentence below, so the
// red-at-trunk / green-at-HEAD matrix is a matrix of test RESULTS.
//
// It is deliberately the weakest assertion in the file (a field exists and
// loadConfig fills it). The behaviour is pinned by the typed tests above; do not
// read this one as covering it.
func TestConfigCarriesAnExcludeListAtAll(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := "storage:\n  backend: \"s3\"\n  exclude_prefixes:\n    - \"u/\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing fixture config: %v", err)
	}
	config, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}

	field := reflect.ValueOf(config.Storage).FieldByName("ExcludePrefixes")
	if !field.IsValid() {
		t.Fatal("StorageConfig has no ExcludePrefixes field, so storage.exclude_prefixes in " +
			"config.yaml is parsed and silently discarded — every user upload written under " +
			"u/ reaches the wall display")
	}
	got, ok := field.Interface().([]string)
	if !ok {
		t.Fatalf("StorageConfig.ExcludePrefixes is %s, want []string", field.Type())
	}
	if len(got) != 1 || got[0] != "u/" {
		t.Fatalf("loadConfig did not carry storage.exclude_prefixes through: got %v, want [u/]", got)
	}
}

// TestLoadConfigLeavesExcludePrefixesUnsetWhenAbsent: the repo's committed
// config.yaml has no exclude_prefixes key, and must keep behaving as it does
// today.
func TestLoadConfigLeavesExcludePrefixesUnsetWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := "" +
		"storage:\n" +
		"  backend: \"s3\"\n" +
		"  bucket: \"comic-flex\"\n" +
		"  prefix: \"\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing fixture config: %v", err)
	}

	config, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if len(config.Storage.ExcludePrefixes) != 0 {
		t.Fatalf("config.Storage.ExcludePrefixes = %v for a config with no such key, want empty",
			config.Storage.ExcludePrefixes)
	}
	if err := validateExcludePrefixes(config.Storage.ExcludePrefixes); err != nil {
		t.Fatalf("an absent exclude list must validate, got %v", err)
	}
}
