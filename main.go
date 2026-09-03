package main

import (
	"context"
	crand "crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ZacxDev/comic-flex/internal/layout"
	"github.com/gotk3/gotk3/gdk"
	"github.com/gotk3/gotk3/glib"
	"github.com/gotk3/gotk3/gtk"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"gopkg.in/yaml.v2"
)

type ViewMode int

const (
	ViewLandscapeSingle ViewMode = iota
	ViewPortraitSingle
	ViewLandscapeTwo
)

type Entry struct {
	ID          string `yaml:"id"`
	Title       string `yaml:"title"`
	ImagePath   string `yaml:"image_path"`
	Description string `yaml:"short_description"`
}

type Manifest struct {
	Entries []Entry `yaml:"entries"`
}

type StorageConfig struct {
	Backend    string `yaml:"backend"`     // "filesystem" or "s3"
	Endpoint   string `yaml:"endpoint"`    // S3 endpoint (e.g., s3.homelab.lan)
	Bucket     string `yaml:"bucket"`      // S3 bucket name
	Prefix     string `yaml:"prefix"`      // Optional prefix/folder in bucket
	UseSSL     bool   `yaml:"use_ssl"`     // HTTPS (true for external)
	SkipVerify bool   `yaml:"skip_verify"` // Skip TLS certificate verification (for self-signed certs)
}

type Config struct {
	ContentDirectory string        `yaml:"content_directory"`
	ManifestPath     string        `yaml:"manifest_path"`
	SlideInterval    uint          `yaml:"slide_interval"`
	FillColor        string        `yaml:"fill_color"`
	TextColor        string        `yaml:"text_color"`
	EnableText       bool          `yaml:"enable_text"`
	IsRandomOrder    bool          `yaml:"is_random_order"`
	ViewMode         string        `yaml:"view_mode"`
	Storage          StorageConfig `yaml:"storage"`
}

// ImageStore abstracts image storage backend
type ImageStore interface {
	ListImages() ([]string, error)
	LoadImage(key string) (*gdk.Pixbuf, error)
}

// FileSystemStore implements ImageStore for local filesystem
type FileSystemStore struct {
	contentDir string
}

func NewFileSystemStore(contentDir string) *FileSystemStore {
	return &FileSystemStore{contentDir: contentDir}
}

func (fs *FileSystemStore) ListImages() ([]string, error) {
	var images []string
	err := filepath.Walk(fs.contentDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			switch strings.ToLower(filepath.Ext(path)) {
			case ".jpg", ".jpeg", ".png", ".gif", ".bmp":
				images = append(images, path)
			}
		}
		return nil
	})
	return images, err
}

func (fs *FileSystemStore) LoadImage(path string) (*gdk.Pixbuf, error) {
	return gdk.PixbufNewFromFile(path)
}

// S3Store implements ImageStore for S3/Minio
type S3Store struct {
	client *minio.Client
	bucket string
	prefix string
}

func NewS3Store(endpoint, bucket, prefix string, useSSL, skipVerify bool) (*S3Store, error) {
	accessKey := os.Getenv("AWS_ACCESS_KEY_ID")
	secretKey := os.Getenv("AWS_SECRET_ACCESS_KEY")

	if accessKey == "" || secretKey == "" {
		return nil, fmt.Errorf("AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY environment variables are required")
	}

	opts := &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: useSSL,
	}

	// Configure custom transport for self-signed certificates
	if skipVerify {
		opts.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
		}
	}

	client, err := minio.New(endpoint, opts)
	if err != nil {
		return nil, fmt.Errorf("failed to create S3 client: %w", err)
	}

	return &S3Store{
		client: client,
		bucket: bucket,
		prefix: prefix,
	}, nil
}

// listTimeout bounds a bucket listing.
//
// 🔴 There was none. ListImages used a bare context.Background(), unlike
// LoadImage's 30 s below, so a MinIO that accepted the connection and then went
// quiet parked the scanning goroutine FOREVER: `scanning` never cleared, the
// display never got its first image, and — once POST /api/rescan exists — every
// retry leaked another goroutine and another concurrent ListObjects. A listing
// is many round trips rather than one GET, so it gets a longer budget than
// LoadImage, but it gets one.
const listTimeout = 2 * time.Minute

func (s *S3Store) ListImages() ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), listTimeout)
	defer cancel()
	var images []string

	objectCh := s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{
		Prefix:    s.prefix,
		Recursive: true,
	})

	for obj := range objectCh {
		if obj.Err != nil {
			return nil, obj.Err
		}
		ext := strings.ToLower(filepath.Ext(obj.Key))
		switch ext {
		case ".jpg", ".jpeg", ".png", ".gif", ".bmp":
			images = append(images, obj.Key)
		}
	}

	return images, nil
}

func (s *S3Store) LoadImage(key string) (*gdk.Pixbuf, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get object %s: %w", key, err)
	}
	defer obj.Close()

	data, err := io.ReadAll(obj)
	if err != nil {
		return nil, fmt.Errorf("failed to read object %s: %w", key, err)
	}

	// Use PixbufLoader to safely load image from bytes
	// This avoids the memory corruption bug in PixbufNewFromData
	loader, err := gdk.PixbufLoaderNew()
	if err != nil {
		return nil, fmt.Errorf("failed to create pixbuf loader: %w", err)
	}

	if _, err := loader.Write(data); err != nil {
		loader.Close()
		return nil, fmt.Errorf("failed to write to pixbuf loader: %w", err)
	}

	if err := loader.Close(); err != nil {
		return nil, fmt.Errorf("failed to close pixbuf loader: %w", err)
	}

	pixbuf, err := loader.GetPixbuf()
	if err != nil {
		return nil, fmt.Errorf("failed to get pixbuf: %w", err)
	}

	return pixbuf, nil
}

// NewImageStore creates the appropriate ImageStore based on config
func NewImageStore(config *Config) (ImageStore, error) {
	switch config.Storage.Backend {
	case "s3":
		return NewS3Store(
			config.Storage.Endpoint,
			config.Storage.Bucket,
			config.Storage.Prefix,
			config.Storage.UseSSL,
			config.Storage.SkipVerify,
		)
	default: // "filesystem" or empty
		return NewFileSystemStore(config.ContentDirectory), nil
	}
}

type ImageViewer struct {
	store        ImageStore
	images       []string
	currentIndex int
	mutex        *sync.RWMutex
	window       *gtk.Window
	image        *gtk.Image
	config       *Config
	timeoutID    glib.SourceHandle
	paused       bool
	viewMode     ViewMode
	// scansInFlight counts the SCANS outstanding — each one a bucket listing plus
	// the display callback it schedules, which is why the slot is released inside
	// that callback and not when the listing returns. Non-zero is what lets
	// GET /api/state tell "not yet scanned" (total 0, indexing) apart from
	// "scanned and empty" (total 0, no comics); note that it also stays non-zero
	// AFTER total is populated, until the callback runs. It is a COUNT and not a
	// flag because scans overlap — see tryBeginScan in state.go. Guarded by mutex
	// like the rest.
	scansInFlight int
	// queuedMutations counts control-API closures handed to the GTK main loop
	// and not yet run. Guarded by mutex like the rest; see maxQueuedMutations.
	queuedMutations int
	// scanRefusals, queueRefusals and admissionRefusals coalesce the log lines the
	// THREE admission points write when they refuse — the concurrent-scan bound,
	// the GTK command queue cap, and internal/control's gallery-not-yet-indexed
	// gate on POST /api/queue, which is decided in that package and reported back
	// through the RefuseLog callback startControlAPI installs. They carry their
	// OWN mutex (see refusalLog in state.go) and are deliberately not guarded by
	// iv.mutex: a refusal must not contend for the lock every render takes.
	scanRefusals      refusalLog
	queueRefusals     refusalLog
	admissionRefusals refusalLog
	// displayUnknown coalesces the line displaySize writes when GDK cannot tell
	// us the monitor geometry. Same reason and same mechanism as the two above:
	// it is reached from every render, so it must not be one line per call.
	displayUnknown refusalLog
	// lastLayoutW/H is the box the last COMPLETED render scaled into. It is what
	// makes the configure-event relayout idempotent: the geometry a rotation
	// produces arrives as a burst of events, and only a box that differs from
	// this one is worth a fresh 30 s image load. Zero means "nothing rendered
	// yet", which correctly compares unequal to any real box.
	lastLayoutBox layout.Box
	// relayoutPending is the single-slot bound on relayout closures queued on the
	// GTK main loop. See beginRelayout, and maxQueuedMutations for why a new
	// scheduler onto that loop has to declare its bound.
	relayoutPending bool
	// displayedKeys is what the last COMPLETED render put on the screen, left to
	// right — one key in either single view, two in the two-up view, one in the
	// two-up view when both halves are the same position. It is written by the
	// render paths AFTER SetFromPixbuf, for exactly the reason lastLayoutBox is:
	// a render that bailed out must leave the previous frame's answer standing,
	// because that frame is still lit.
	//
	// 🔴 It is what GET /api/state's `keys` reports, and it exists so that a
	// consumer never has to DERIVE the second two-up key from currentIndex. It
	// cannot: the gallery is shuffled per process (is_random_order), so the
	// client's ordering is not this one. Guarded by mutex like the rest.
	displayedKeys []string
	// nextAdvanceAt is when the armed slide timer will next fire, in Go's
	// monotonic clock. It is written by the SAME call that stores timeoutID
	// (swapTimeout), from the SAME interval that arms the GLib source, so the
	// countdown GET /api/state reports cannot drift away from the timer that
	// drives it. Zero means nothing is armed. Guarded by mutex like the rest.
	nextAdvanceAt time.Time
	// armTimer is startSlideshow's own startTimer closure, kept so that
	// POST /api/interval can run it AGAIN — retiring the pending GLib source and
	// arming a fresh one at the new interval — instead of leaving the display
	// waiting out the OLD interval and the countdown frozen against it.
	//
	// 🔴 It is a handle on the SINGLE arming site, not a second one. Everything
	// seconds_until_next's honesty rests on — one interval read feeding both the
	// GLib source and the deadline, one atomic swapTimeout writing the handle and
	// the deadline together — lives in that closure, so the re-arm inherits it by
	// construction. A field holding a "arm a timer" convenience written anywhere
	// else would be the two-clocks defect wearing a struct field.
	//
	// The FIELD is guarded by mutex like the rest; what it points at must run on
	// the GTK main loop. See setArmTimer and rearmSlideTimer in state.go.
	armTimer func()
	// queue is the play queue: an ordered list of object keys POST /api/queue
	// asked for, to be shown BEFORE the shuffled gallery resumes. Empty means no
	// queue is running. Guarded by mutex like the rest.
	//
	// 🔴 TRANSIENT BY DESIGN (decision D2) — it lives here and nowhere else, and
	// a restarted Pi comes back without it. See the play-queue section of
	// state.go, which owns every access to the SIX queue fields below (queue,
	// queueIndex, queueTailIndex, queueScanned, queueSkipped, queueSeq) plus the
	// two that record the interruption point (queueReturnIndex, queueReturnKey).
	// Count them rather than trusting this sentence — it said "five" for a round
	// after three more were added.
	queue []string
	// queueIndex is the cursor into queue: the LEFT-most entry currently on the
	// display. -1 means none is — either no queue is running, or one was
	// installed and no page turn has consumed an entry yet. The wire's 1-based
	// `queue.position` is derived from it in queueStateLocked, in one place.
	queueIndex int
	// queueTailIndex is the LAST entry on the display: queueIndex in either
	// single view, and the entry sharing the screen with it in the two-up view.
	// -1 alongside queueIndex.
	//
	// 🔴 It is what makes the two-up view play the QUEUE rather than the queue on
	// the left and the gallery on the right. pairKeys reads it; landQueueLocked
	// is the only writer, and it writes it in the same lock acquisition that moves
	// the cursor.
	queueTailIndex int
	// queueScanned is the high-water mark of the forward scan: every entry below
	// it has already been looked up in the gallery once. It is what makes a
	// skipped key count EXACTLY ONCE no matter how often the operator steps back
	// and forward over it.
	queueScanned int
	// queueSkipped counts entries passed over because they were no longer in the
	// gallery (decision D3). It OUTLIVES the queue that produced it and is reset
	// only by the next setQueue — a drained queue reports length 0, so a count
	// cleared with the queue could never be read by the client it exists for.
	queueSkipped int
	// queueReturnIndex and queueReturnKey are the gallery position and the PAGE
	// the queue interrupted, restored when the queue drains (decision D4). The
	// queue is an interruption, not a seek.
	//
	// 🔴 Two of them because a rescan under a running queue reshuffles the
	// gallery, and an index then names a different comic. The key wins when it is
	// still in the bucket; the index is the fallback for a page that has left it.
	// queueResumeIndexLocked is the one place that decides between them.
	queueReturnIndex int
	queueReturnKey   string
	// queueSeq is the play queue's generation: incremented by every setQueue,
	// never reused, 0 before the first one.
	//
	// 🔴 It is what makes queueSkipped ATTRIBUTABLE. That count deliberately
	// outlives the queue that produced it, so without an identity beside it a
	// polling client cannot tell "the collection that just finished skipped 2"
	// from "some queue an hour ago skipped 2" — and would show a stale toast on
	// every poll forever. It is reported as `queue.id`. Being per-process, it
	// also restates decision D2 on the wire: a Pi that restarted is back at 0.
	queueSeq int
}

// injectedVersion is set at link time by a release build:
//
//	go build -ldflags "-X main.injectedVersion=0.2.0"
//
// It is empty in an ordinary build, and resolveVersion then falls back to the
// VCS stamp the toolchain embeds.
var injectedVersion string

// version is reported by GET /healthz.
//
// 🔴 It is deliberately NOT a hand-maintained constant. It used to be
// `const version = "0.2.0"`, with nothing in the build bumping it — so /healthz
// would have gone on reporting 0.2.0 for every subsequent deploy that nobody
// remembered to hand-edit, which is worse than reporting nothing. resolveVersion
// prefers the link-time value and otherwise derives one from the commit the
// binary was built from.
//
// 🔴 SCOPE, corrected. This comment used to end "so an un-edited build still
// identifies itself", full stop. That holds for a `go build` in a git clone —
// which is how the Pi builds — and it did NOT hold for the repo's own
// `nix build .#default`: buildGoModule copies the source into /nix/store with
// no .git, so -buildvcs=auto stamps nothing, the VCS branch below finds no
// revision, and this resolves to "unknown". Nothing caught it, because
// TestVersionIsNotEmpty passes on "unknown" — it is not empty.
//
// The fix is in flake.nix, which now injects `-X main.injectedVersion=<rev>`,
// so the FIRST branch of resolveVersion answers for a nix build and the VCS
// branch answers for a plain clone. Both paths now identify the binary; if you
// add a third build path, give it one of the two or it lands on "unknown"
// silently.
var version = resolveVersion(injectedVersion, buildSettings())

// bootID identifies THIS RUN of the process. It is reported by GET /api/state as
// `boot_id`, and it is generated once, here, at package initialisation.
//
// 🔴 It exists so a client can dedupe on the PAIR (boot_id, queue.id). queue.id
// is a per-process counter that starts again at 0 after a restart, so ids are
// reused across boots — a client that suppressed a "3 pages were no longer in the
// library" notification because it had "already handled queue 3" would go on
// suppressing a genuine one from a rebooted Pi's third collection. It also gives
// a client the only way to observe decision D2 from outside: a changed boot_id is
// how it learns the collection it was watching is GONE rather than finished.
//
// It is not a version and not ordered — comparing two values for anything but
// equality is meaningless.
var bootID = newBootID()

// newBootID returns an opaque per-run identity.
//
// 🔴 Random, not the clock, and the fallback ordering is the reason. A Raspberry
// Pi has NO BATTERY-BACKED RTC: it comes up at whatever time the filesystem or
// NTP last left it, so two consecutive boots can genuinely report the same wall
// clock, and a time-derived identity would then be equal across exactly the event
// it exists to distinguish. crypto/rand is the primary; the clock is only the
// last resort for a machine on which reading randomness has failed, where an
// occasionally-repeated id is still better than an empty field a client cannot
// branch on.
func newBootID() string {
	var b [8]byte
	if _, err := crand.Read(b[:]); err != nil {
		log.Printf("boot id: crypto/rand unavailable (%v); falling back to the clock, which on "+
			"a Pi with no RTC can repeat across a reboot", err)
		return "t" + strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b[:])
}

// buildSettings returns the toolchain's VCS stamp, or nil when there is none
// (ReadBuildInfo has no build info, or the build was made with -buildvcs=false).
func buildSettings() []debug.BuildSetting {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return nil
	}
	return info.Settings
}

// resolveVersion picks the most specific identifier available: an explicit
// link-time value, else the VCS revision (short, with a -dirty marker), else
// "unknown". It takes both inputs as parameters so it is testable without
// rebuilding the binary under different flags.
func resolveVersion(injected string, settings []debug.BuildSetting) string {
	if injected != "" {
		return injected
	}
	revision, dirty := "", false
	for _, s := range settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if revision == "" {
		return "unknown"
	}
	if len(revision) > 12 {
		revision = revision[:12]
	}
	if dirty {
		return revision + "-dirty"
	}
	return revision
}

func loadManifest(path string) (*Manifest, error) {
	var manifest Manifest

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	err = yaml.Unmarshal(data, &manifest)
	if err != nil {
		return nil, err
	}

	return &manifest, nil
}

func loadConfig(path string) (*Config, error) {
	var config Config

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	err = yaml.Unmarshal(data, &config)
	if err != nil {
		return nil, err
	}

	// Set default values
	if config.ContentDirectory == "" {
		config.ContentDirectory = "./content"
	}
	if config.SlideInterval == 0 {
		config.SlideInterval = 30
	}
	if config.FillColor == "" {
		config.FillColor = "#ADD8E6"
	}
	if config.TextColor == "" {
		config.TextColor = "#000000"
	}
	if config.ManifestPath == "" {
		config.ManifestPath = "./manifest.yaml"
	}
	if config.ViewMode == "" {
		config.ViewMode = "landscape_single"
	}

	return &config, nil
}

func parseViewMode(s string) ViewMode {
	switch s {
	case "portrait_single":
		return ViewPortraitSingle
	case "landscape_two":
		return ViewLandscapeTwo
	default:
		return ViewLandscapeSingle
	}
}

func hexToRGB(hexColor string) (float64, float64, float64, error) {
	var r, g, b uint8
	_, err := fmt.Sscanf(hexColor, "#%02x%02x%02x", &r, &g, &b)
	if err != nil {
		return 0, 0, 0, err
	}
	return float64(r) / 255.0, float64(g) / 255.0, float64(b) / 255.0, nil
}

func NewImageViewer(config *Config, store ImageStore) (*ImageViewer, error) {
	win, err := gtk.WindowNew(gtk.WINDOW_TOPLEVEL)
	if err != nil {
		return nil, err
	}

	img, err := gtk.ImageNew()
	if err != nil {
		return nil, err
	}

	return &ImageViewer{
		store:  store,
		images: make([]string, 0),
		mutex:  &sync.RWMutex{},
		window: win,
		image:  img,
		config: config,
	}, nil
}

// scanImagesAsync starts a bucket listing in the background and reports whether
// one was started. It returns false, having started nothing, when
// maxConcurrentScans scans are already outstanding.
//
// 🔴 The bool is the ADMISSION DECISION for POST /api/rescan, and it is answered
// SYNCHRONOUSLY on the caller's goroutine. That is why the handler does not
// enqueue this onto the GTK main loop: nothing here touches a widget (the
// goroutine's only GTK contact is the scheduler below, which is exactly how a
// non-GTK thread is supposed to reach the loop), and routing it through
// enqueueBounded made the queue cap LOOK like it bounded rescans while the
// closure completed in microseconds and the listing it spawned ran free. See
// maxConcurrentScans in state.go for the measurement.
func (iv *ImageViewer) scanImagesAsync() bool {
	return iv.scanImagesAsyncVia(idleOnce)
}

// scanImagesAsyncVia is scanImagesAsync with the GTK-main-loop scheduler as a
// parameter, exactly as enqueueBounded takes one and for the same reason: the
// property that matters here is WHEN THE SLOT IS RETURNED, and that now happens
// inside the scheduled closure, which no test can observe without either a
// running GTK main loop or a stand-in scheduler.
//
// 🔴 THE SLOT SPANS THE WHOLE SCAN, INCLUDING THE COMPLETION CLOSURE. It used to
// be returned by a `defer iv.endScan()` in the goroutine — i.e. as soon as the
// completion closure had been SCHEDULED rather than when it RAN. That is round
// 1's defect one level down: a bound that frees its slot before the work it
// bounds completes. Measured at 99a9dca, 40 sequential admitted rescans with
// maxConcurrentScans=4:
//
//	admitted=40 refused=0 queueDepth=0 -> 40 scan-completion closures
//	outstanding on the GTK main context at once
//
// Each of those closures re-enters onScanComplete -> updateImage -> LoadImage,
// so N rescans bought N serialized 30 s image loads and N gotk3
// callback-registry entries — exactly the growth maxQueuedMutations exists to
// prevent, on the one endpoint that was supposed to be bounded by
// maxConcurrentScans. Releasing in the closure makes maxConcurrentScans bound
// what its comment claims: listings AND their completion closures.
//
// 🔴 The one way a slot can now be lost is a closure that is scheduled and never
// runs, i.e. the main loop stopping (shutdown). That is deliberate and it is the
// SAME behaviour maxQueuedMutations already has — releaseQueueSlot also only
// runs from inside the scheduled closure — and it is the honest answer: a main
// loop that is never going to run another closure is not going to display a
// rescan's results either, so refusing is correct rather than a leak. What is
// NOT allowed is losing a slot on a path that terminates for any other reason,
// which is why the release below is a sync.Once reached from both the error
// return and the closure.
func (iv *ImageViewer) scanImagesAsyncVia(schedule func(func())) bool {
	// Counted synchronously, before the goroutine starts. Counting it inside the
	// goroutine would leave a window in which GET /api/state reports
	// scanning:false with total 0 — the "no comics" answer — for a gallery that
	// is about to be listed.
	//
	// 🔴 tryBeginScan/endScan are a COUNTER. Two listings genuinely overlap here:
	// POST /api/rescan can arrive during the startup scan, or twice in a row.
	// A boolean flag let whichever goroutine returned first clear it while the
	// other was still listing.
	if !iv.tryBeginScan() {
		// 🔴 COALESCED, not one line per refusal. This is the DoS-adjacent path:
		// an authenticated client that loops POST /api/rescan was turning its own
		// backpressure into unbounded journald volume on a Raspberry Pi — 496
		// lines from a single test run. See refusalLog in state.go.
		// Depth read BEFORE note() — see the same pattern in enqueueBounded.
		outstanding := iv.scanCount()
		if n, since, report := iv.scanRefusals.note(time.Now()); report {
			log.Printf("rescan refused: %d scans already outstanding (max %d); "+
				"%d refusal(s) %s", outstanding, maxConcurrentScans, n, refusalSpan(since))
		}
		return false
	}

	go func() {
		// Exactly one release, on exactly one of the two paths that can end this
		// scan. sync.Once makes a double release impossible (it would hand out a
		// slot nobody holds) and makes the missing-release case a held slot
		// rather than a silently negative counter.
		var once sync.Once
		release := func() { once.Do(iv.endScan) }

		images, err := iv.store.ListImages()
		if err != nil {
			log.Printf("Error listing images: %v", err)
			release()
			return
		}

		if iv.config.IsRandomOrder {
			rdm := rand.New(rand.NewSource(time.Now().UnixNano()))
			rdm.Shuffle(len(images), func(i, j int) {
				images[i], images[j] = images[j], images[i]
			})
		}

		iv.setImages(images)

		// Update the first image if none is showing. schedule is idleOnce in
		// production, the program's single glib.IdleAdd call site — see
		// control_adapter.go.
		// 🔴 This scheduling is OUTSIDE enqueueBounded's accounting, deliberately
		// and unavoidably — the listing goroutine has no HTTP response left to
		// refuse with. It is bounded instead by maxConcurrentScans, and that is
		// true only because the slot is released BELOW, when this closure runs.
		schedule(func() {
			defer release()
			iv.onScanComplete(iv.updateImage)
		})
	}()
	return true
}

func (iv *ImageViewer) updateImage() {
	switch iv.getViewMode() {
	case ViewLandscapeSingle, ViewPortraitSingle:
		iv.updateSingleImage()
	case ViewLandscapeTwo:
		iv.updateTwoImages()
	}
}

func (iv *ImageViewer) updateSingleImage() {
	idx, imageKey, ok := iv.currentKey()
	if !ok {
		return
	}

	log.Printf("Loading image %d: %s", idx, imageKey)
	pixbuf, err := iv.store.LoadImage(imageKey)
	if err != nil {
		log.Printf("Unable to load image: %v", err)
		return
	}

	box := iv.layoutBox()

	scaledPixbuf, err := scaleToFit(pixbuf, box.W, box.H)
	if err != nil {
		log.Printf("Unable to scale pixbuf: %v", err)
		return
	}

	iv.image.SetFromPixbuf(scaledPixbuf)
	// Recorded only after the pixbuf is actually on the widget, so that a render
	// which bailed out above is retried by the next geometry change rather than
	// being remembered as done. noteDisplayed is here for the same reason and
	// must stay here: a render that returned early above left the PREVIOUS comic
	// on the screen, and that is what GET /api/state must keep reporting.
	iv.noteLayoutBox(box)
	iv.noteDisplayed([]string{imageKey})
	runtime.GC()
}

func (iv *ImageViewer) updateTwoImages() {
	idx, leftKey, rightIdx, rightKey, ok := iv.pairKeys()
	if !ok {
		return
	}

	log.Printf("Loading two images: %d (%s) and %d (%s)", idx, leftKey, rightIdx, rightKey)

	// Load both images in parallel
	var leftPb, rightPb *gdk.Pixbuf
	var leftErr, rightErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		leftPb, leftErr = iv.store.LoadImage(leftKey)
	}()
	go func() {
		defer wg.Done()
		rightPb, rightErr = iv.store.LoadImage(rightKey)
	}()
	wg.Wait()

	if leftErr != nil {
		log.Printf("Unable to load left image: %v", leftErr)
		return
	}
	if rightErr != nil {
		log.Printf("Unable to load right image: %v", rightErr)
		return
	}

	// Layout: [margin][left image][gap][right image][margin], each image centred
	// vertically in the canvas. All of that arithmetic is layout.TwoUp now, so it
	// is testable without a display — and so this path and updateSingleImage read
	// the SAME box from the SAME place.
	box := iv.layoutBox()
	plan := layout.TwoUp(box, leftPb.GetWidth(), leftPb.GetHeight(), rightPb.GetWidth(), rightPb.GetHeight())

	// A box too narrow to hold two images either side of the gap leaves a
	// non-positive scale target. ScaleSimple does return an error for that, but
	// only after gdk_pixbuf emits a g_return_val_if_fail critical, so say it
	// plainly here instead.
	if plan.Left.ScaleW < 1 || plan.Left.ScaleH < 1 || plan.Right.ScaleW < 1 || plan.Right.ScaleH < 1 {
		log.Printf("Unable to lay two images out in a %dx%d box", box.W, box.H)
		return
	}

	// Scale both images to fit within their allocated space.
	// Never crop - always scale to fit entirely within bounds.
	leftScaled, err := leftPb.ScaleSimple(plan.Left.ScaleW, plan.Left.ScaleH, gdk.INTERP_BILINEAR)
	if err != nil {
		log.Printf("Unable to scale left image: %v", err)
		return
	}
	rightScaled, err := rightPb.ScaleSimple(plan.Right.ScaleW, plan.Right.ScaleH, gdk.INTERP_BILINEAR)
	if err != nil {
		log.Printf("Unable to scale right image: %v", err)
		return
	}

	// Create black canvas
	canvas, err := gdk.PixbufNew(gdk.COLORSPACE_RGB, true, 8, plan.CanvasW, plan.CanvasH)
	if err != nil {
		log.Printf("Unable to create canvas: %v", err)
		return
	}
	canvas.Fill(0x000000FF)

	// Composite images onto canvas with bounds checking.
	// dest_x/y: where to start writing in dest
	// dest_width/height: how many pixels to write (clamped to canvas bounds)
	// offset_x/y: maps source (0,0) to this dest coordinate
	if plan.Left.Visible() {
		leftScaled.Composite(canvas, plan.Left.X, plan.Left.Y, plan.Left.DestW, plan.Left.DestH,
			float64(plan.Left.X), float64(plan.Left.Y), 1.0, 1.0, gdk.INTERP_BILINEAR, 255)
	}
	if plan.Right.Visible() {
		rightScaled.Composite(canvas, plan.Right.X, plan.Right.Y, plan.Right.DestW, plan.Right.DestH,
			float64(plan.Right.X), float64(plan.Right.Y), 1.0, 1.0, gdk.INTERP_BILINEAR, 255)
	}

	iv.image.SetFromPixbuf(canvas)
	iv.noteLayoutBox(box)
	// The keys THIS render loaded and composited, from the same pairKeys read the
	// loads above used — not a fresh look at currentIndex, which a queued page
	// turn may already have moved while the two S3 GETs were in flight.
	iv.noteDisplayed(displayedPair(idx, leftKey, rightIdx, rightKey))
	runtime.GC()
}

// scaleToFit scales a pixbuf to the largest size that fits the box, preserving
// its aspect ratio.
//
// The arithmetic now lives in internal/layout so it can be tested without a
// display; this is the gdk_pixbuf call that was wrapped around it. See
// layout.Fit for what changed (two guards) and what did not (everything else).
func scaleToFit(pb *gdk.Pixbuf, maxWidth, maxHeight int) (*gdk.Pixbuf, error) {
	destW, destH := layout.Fit(pb.GetWidth(), pb.GetHeight(), layout.Box{W: maxWidth, H: maxHeight})
	if destW == 0 || destH == 0 {
		return nil, fmt.Errorf("cannot scale a %dx%d pixbuf into a %dx%d box",
			pb.GetWidth(), pb.GetHeight(), maxWidth, maxHeight)
	}
	return pb.ScaleSimple(destW, destH, gdk.INTERP_BILINEAR)
}

// displaySize returns the pixel geometry of the monitor the window is on, or
// 0,0 when GDK cannot tell us.
//
// 🔴 It deliberately does NOT use gdk.Screen.GetWidth/GetHeight, which look like
// the obvious answer: at the pinned gotk3 v0.6.2 those live in
// gdk_deprecated_since_3_22.go, behind a `gtk_deprecated` build tag that this
// module does not set, so naming them does not compile. The monitor API in
// gdk_since_3_22.go is what is actually built, and the Pi runs GTK 3.24.
//
// Returning 0,0 rather than guessing is what makes the failure safe: SelectBox
// treats a non-positive dimension as unknown and falls back to the window size,
// which is exactly the behaviour that shipped before this change.
func (iv *ImageViewer) displaySize() (int, int) {
	display, err := gdk.DisplayGetDefault()
	if err != nil || display == nil {
		iv.noteDisplayUnknown("no default GDK display")
		return 0, 0
	}

	w, h, ok := firstUsableSize(
		// The monitor the window is actually on, when the window is realised.
		func() (int, int) {
			gdkWin, err := iv.window.GetWindow()
			if err != nil || gdkWin == nil {
				return 0, 0
			}
			monitor, err := display.GetMonitorAtWindow(gdkWin)
			if err != nil || monitor == nil {
				return 0, 0
			}
			return monitorGeometry(monitor)
		},
		// Before the window is realised there is no monitor-at-window to ask for.
		func() (int, int) {
			monitor, err := display.GetPrimaryMonitor()
			if err != nil || monitor == nil {
				return 0, 0
			}
			return monitorGeometry(monitor)
		},
		func() (int, int) {
			if display.GetNMonitors() <= 0 {
				return 0, 0
			}
			monitor, err := display.GetMonitor(0)
			if err != nil || monitor == nil {
				return 0, 0
			}
			return monitorGeometry(monitor)
		},
	)
	if ok {
		return w, h
	}

	// 🔴 This is SILENT INERTNESS unless it says something. With no display
	// reading, SelectBox falls back entirely to the window size — which is
	// exactly the policy that latched. The operator would then see the original
	// bug, from a build that claims to fix it, with nothing in the journal.
	iv.noteDisplayUnknown("GDK reported no usable monitor geometry")
	return 0, 0
}

// firstUsableSize returns the first reading whose dimensions are BOTH positive.
//
// 🔴 The fallback chain in displaySize is a function rather than three nested
// ifs because round 1 of the audit found that chain UNREACHABLE. The first
// candidate's guard was `if geo != nil`, and at the pinned gotk3 v0.6.2
// Monitor.GetGeometry is `wrapRectangle(&rect)` over a stack local, with
// wrapRectangle returning nil ONLY for a nil argument — so that pointer is
// never nil and the check was provably always true. An invalidated monitor
// (one being torn down mid-xrandr, i.e. exactly when this runs) leaves the
// rectangle ZEROED, and a 0x0 geometry then sailed through as an answer and
// short-circuited both fallbacks — making them unreachable for the one failure
// they were written for.
//
// Saying "the first USABLE reading" once, in a function with a test, is what
// stops that shape returning. Readings are funcs so a later candidate is never
// evaluated once an earlier one answers.
func firstUsableSize(readings ...func() (int, int)) (w, h int, ok bool) {
	for _, read := range readings {
		w, h = read()
		if w > 0 && h > 0 {
			return w, h, true
		}
	}
	return 0, 0, false
}

// monitorGeometry reads a monitor's pixel geometry. It reports 0,0 when GDK
// hands back nothing usable; firstUsableSize decides what that means.
//
// ⚠ The nil check below is UNREACHABLE at the pinned gotk3 v0.6.2 and no test
// pins it — said plainly so nobody reads it as coverage. GetGeometry is
// `wrapRectangle(&rect)` over a stack local and wrapRectangle returns nil only
// for a nil argument, so the pointer is always non-nil. It is kept as defence
// against that changing, NOT as the guard against a bad reading: the guard that
// actually fires is the positive-dimension test in firstUsableSize, which is
// what a zeroed rectangle trips.
func monitorGeometry(monitor *gdk.Monitor) (int, int) {
	geo := monitor.GetGeometry()
	if geo == nil {
		return 0, 0
	}
	return geo.GetWidth(), geo.GetHeight()
}

// noteDisplayUnknown logs that the display geometry could not be read, at most
// once per refusalLogInterval.
//
// It is coalesced through the same refusalLog type the three admission points
// use, because displaySize runs on every render and every relayout: a line per call
// would be unbounded journald volume on a Raspberry Pi for a condition that is
// one condition, not many.
func (iv *ImageViewer) noteDisplayUnknown(reason string) {
	if n, since, report := iv.displayUnknown.note(time.Now()); report {
		log.Printf("display geometry unavailable (%s): layout is falling back to the window "+
			"size, which cannot correct an oversized window; %d occurrence(s) %s",
			reason, n, refusalSpan(since))
	}
}

// layoutBox returns the box every render must scale into.
//
// 🔴 This replaces the bare `iv.window.GetSize()` that both render paths used.
// The window's size is an OUTPUT of the previous render — an oversized pixbuf
// grows it — so feeding it back in as the only input is what let one stale
// frame latch permanently. See layout.SelectBox for the measurement and the
// rule. Both reads happen here, on the GTK thread, and neither takes iv.mutex.
func (iv *ImageViewer) layoutBox() layout.Box {
	displayW, displayH := iv.displaySize()
	windowW, windowH := iv.window.GetSize()
	return layout.SelectBox(displayW, displayH, windowW, windowH)
}

// scheduleRelayout re-renders the current page if the layout box has changed
// since the last render, and reports whether it scheduled anything.
//
// 🔴 It is what makes the fix complete rather than half of one. Fixing the box
// alone still leaves ONE wrong frame after every rotation: xrandr returns before
// the X server has resized the screen and long before GTK has reconfigured the
// window, so the render setViewMode does immediately afterwards is computed
// against pre-rotation geometry no matter which box policy it uses. Measured on
// the Pi, the window did not reach its new size until ~2 s after the POST. There
// was no configure-event or size-allocate handler anywhere in the program, so
// nothing ever recomputed when the real geometry finally arrived.
//
// It must NOT run the render inline. configure-event is emitted during size
// allocation, and updateImage can block for up to 30 s in an S3 GET; doing that
// inside GTK's own layout pass is a reentrancy hazard as well as a freeze. So
// the work goes onto the main loop via the program's single glib.IdleAdd site.
//
// 🔴 It is bounded by ONE outstanding relayout, not by maxQueuedMutations — see
// beginRelayout in state.go. A rotation emits a burst of configure events and
// each one would otherwise queue another 30 s image load. The slot is released
// from INSIDE the closure, for the reason scanImagesAsyncVia spells out: a bound
// released when the work is merely SCHEDULED bounds nothing.
//
// schedule is idleOnce in production, and a parameter for the same reason
// enqueueBounded takes one — so both the coalescing and the release are
// exercisable without a GTK main loop.
func (iv *ImageViewer) scheduleRelayout(schedule func(func())) bool {
	return iv.scheduleRelayoutVia(schedule, iv.layoutBox)
}

// scheduleRelayoutVia is scheduleRelayout with both GTK-bound collaborators as
// parameters — the main-loop scheduler and the geometry read.
//
// readBox is a parameter for the same reason schedule is: iv.layoutBox calls
// gtk.Window.GetSize and the GDK monitor API, so a test that could not stand it
// in would need a real window on a real display, and the coalescing and release
// behaviour below would go untested. Compare scanImagesAsyncVia.
func (iv *ImageViewer) scheduleRelayoutVia(schedule func(func()), readBox func() layout.Box) bool {
	if !iv.beginRelayout() {
		return false
	}
	schedule(func() {
		defer iv.endRelayout()
		// The box is read HERE, inside the closure, rather than at the configure
		// event that triggered it: several events arrive while this waits its
		// turn, and only the geometry at render time decides anything.
		box := readBox()
		if !iv.layoutBoxChanged(box) {
			return
		}
		iv.updateImage()
	})
	return true
}

func (iv *ImageViewer) stepSize() int {
	if iv.getViewMode() == ViewLandscapeTwo {
		return 2
	}
	return 1
}

func detectDisplayOutput() string {
	out, err := exec.Command("xrandr", "--query").Output()
	if err != nil {
		log.Printf("xrandr query failed: %v", err)
		return "HDMI-1"
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, " connected") {
			fields := strings.Fields(line)
			if len(fields) > 0 {
				return fields[0]
			}
		}
	}
	return "HDMI-1"
}

func setDisplayRotation(portrait bool) {
	output := detectDisplayOutput()
	rotate := "normal"
	if portrait {
		rotate = "left"
	}
	log.Printf("Setting display %s rotation to %s", output, rotate)
	if err := exec.Command("xrandr", "--output", output, "--rotate", rotate).Run(); err != nil {
		log.Printf("xrandr rotate failed: %v", err)
	}
}

func (iv *ImageViewer) setViewMode(mode ViewMode) {
	orientationChanged := iv.setViewModeState(mode)
	log.Printf("View mode set to %d", mode)

	// Rotating the screen shells out to xrandr, so it runs with no lock held.
	if orientationChanged {
		setDisplayRotation(mode == ViewPortraitSingle)
	}

	iv.updateImage()
}

func (iv *ImageViewer) setupUI() error {
	iv.window.SetTitle("CAM COMIC FLEX")
	iv.window.SetDefaultSize(1920, 1080)

	css := `
                window { background-color: black; }
        `

	cssProvider, err := gtk.CssProviderNew()
	if err != nil {
		return fmt.Errorf("unable to create CSS provider: %v", err)
	}
	cssProvider.LoadFromData(css)

	screen, err := gdk.ScreenGetDefault()
	if err != nil {
		return fmt.Errorf("unable to get screen: %v", err)
	}

	gtk.AddProviderForScreen(screen, cssProvider, uint(gtk.STYLE_PROVIDER_PRIORITY_USER))

	iv.window.Fullscreen()

	// Create overlay
	overlay, err := gtk.OverlayNew()
	if err != nil {
		return fmt.Errorf("unable to create overlay: %v", err)
	}

	// Add image to overlay - center aligned
	iv.image.SetVAlign(gtk.ALIGN_CENTER)
	iv.image.SetHAlign(gtk.ALIGN_CENTER)
	iv.image.SetVExpand(true)
	iv.image.SetHExpand(true)
	overlay.Add(iv.image)

	// Create event overlay to capture mouse events (added on top of everything)
	eventOverlay, err := gtk.DrawingAreaNew()
	if err != nil {
		return fmt.Errorf("unable to create event overlay: %v", err)
	}
	eventOverlay.AddEvents(int(gdk.BUTTON_PRESS_MASK | gdk.SCROLL_MASK | gdk.POINTER_MOTION_MASK))
	eventOverlay.SetHExpand(true)
	eventOverlay.SetVExpand(true)

	// Setup event handlers
	iv.window.Connect("destroy", func() {
		gtk.MainQuit()
	})

	// 🔴 The program had NO resize handling at all — no configure-event, no
	// size-allocate — which is the half of the rotation bug that a correct
	// layout box does not fix on its own. setViewMode shells out to xrandr and
	// renders immediately; the X server has not resized the screen yet and the
	// window manager has not reconfigured the window, so that render is computed
	// against the OLD orientation. Without this handler nothing ever recomputed
	// it, and the wrong frame stayed until something else happened to re-render.
	//
	// Returning false lets GTK's own configure handling run. The work itself is
	// deferred onto the main loop and coalesced — see scheduleRelayout, which
	// must not be inlined here.
	iv.window.Connect("configure-event", func(win *gtk.Window, event *gdk.Event) bool {
		iv.scheduleRelayout(idleOnce)
		return false
	})

	// 🔴 configure-event alone is NOT enough, because it only fires when the
	// WINDOW changes — and the two sides of a rotation arrive independently.
	//
	// If the window manager gets there before GDK does, layoutBox reads a
	// half-updated pair: a window already at 2160x3840 and a display GDK still
	// reports as 3840x2160, whose minimum is a SQUARE 2160x2160 box. That
	// renders small and records itself. When GDK then catches up, the pixbuf is
	// SMALLER than the window, so the window does not move, so no further
	// configure-event is emitted and nothing ever re-reads the geometry. The
	// frame stays letterboxed.
	//
	// While playing, the 30 s slide timer papers over it. While PAUSED it does
	// not: startSlideshow gates the re-render behind `if !iv.isPaused()`, so the
	// undersized frame would persist indefinitely — and pausing is exactly what
	// an operator does to look at one page.
	//
	// Listening to the screen closes it: both orderings now end with a relayout,
	// whichever side moved last. Duplicate triggers cost nothing because
	// scheduleRelayout coalesces to one outstanding closure and skips the render
	// when the box has not actually changed.
	//
	// gdk.Screen is deprecated in GTK 3.22+ but these two signals are still
	// emitted in 3.24 (the Pi runs 3.24.38); Connect at gotk3 v0.6.2 returns only
	// a handle, so there is no error to check.
	// Written out rather than looped over a slice of names: these two names ARE
	// the wiring, and TestGeometryChangesAreObservedFromBothTheWindowAndTheScreen
	// pins the set by reading them here. A loop variable hides them from that
	// ledger, which is how the screen half could be deleted with every test
	// still green.
	screen.Connect("size-changed", func() { iv.scheduleRelayout(idleOnce) })
	screen.Connect("monitors-changed", func() { iv.scheduleRelayout(idleOnce) })

	iv.window.Connect("key-press-event", func(win *gtk.Window, event *gdk.Event) {
		keyEvent := &gdk.EventKey{Event: event}
		altPressed := keyEvent.State()&uint(gdk.MOD1_MASK) != 0

		if altPressed {
			switch keyEvent.KeyVal() {
			case gdk.KEY_1:
				iv.setViewMode(ViewPortraitSingle)
				return
			case gdk.KEY_2:
				iv.setViewMode(ViewLandscapeSingle)
				return
			case gdk.KEY_3:
				iv.setViewMode(ViewLandscapeTwo)
				return
			}
		}

		step := iv.stepSize()
		moved := false
		switch keyEvent.KeyVal() {
		case gdk.KEY_space, gdk.KEY_Right:
			moved = iv.advance(step)
		case gdk.KEY_Left:
			moved = iv.advance(-step)
		}
		if moved {
			iv.updateImage()
		}
	})

	// Add event overlay last (on top)
	overlay.AddOverlay(eventOverlay)
	overlay.SetOverlayPassThrough(eventOverlay, false)
	log.Println("Event overlay added")

	eventOverlay.Connect("button-press-event", func(da *gtk.DrawingArea, event *gdk.Event) bool {
		log.Println("Button press detected - toggling pause")
		log.Printf("Paused: %v", iv.togglePaused())
		return true
	})

	eventOverlay.Connect("scroll-event", func(da *gtk.DrawingArea, event *gdk.Event) bool {
		scrollEvent := gdk.EventScrollNewFromEvent(event)
		direction := scrollEvent.Direction()
		log.Printf("Scroll event detected - direction: %v", direction)

		step := iv.stepSize()
		changed := false
		if direction == gdk.SCROLL_UP || direction == gdk.SCROLL_LEFT {
			changed = iv.advance(-step)
		} else if direction == gdk.SCROLL_DOWN || direction == gdk.SCROLL_RIGHT {
			changed = iv.advance(step)
		}
		if changed {
			if idx, _, ok := iv.currentKey(); ok {
				log.Printf("Changed to index: %d", idx)
			}
			iv.updateImage()
		}
		return true
	})

	// Hide cursor when mouse is near bottom of viewport
	var cursorHidden bool
	display, _ := gdk.DisplayGetDefault()
	var blankCursor *gdk.Cursor
	if display != nil {
		blankCursor, _ = gdk.CursorNewFromName(display, "none")
	}

	eventOverlay.Connect("motion-notify-event", func(da *gtk.DrawingArea, event *gdk.Event) bool {
		motionEvent := gdk.EventMotionNewFromEvent(event)
		_, y := motionEvent.MotionVal()
		_, winHeight := iv.window.GetSize()

		// Hide cursor when in bottom 150 pixels (text area)
		bottomThreshold := float64(winHeight - 150)
		gdkWin, err := iv.window.GetWindow()
		if err != nil {
			return false
		}

		if y > bottomThreshold {
			if !cursorHidden && blankCursor != nil {
				gdkWin.SetCursor(blankCursor)
				cursorHidden = true
			}
		} else {
			if cursorHidden {
				gdkWin.SetCursor(nil) // Restore default cursor
				cursorHidden = false
			}
		}
		return false
	})

	iv.window.Add(overlay)
	iv.window.ShowAll()

	return nil
}

func (iv *ImageViewer) startSlideshow() {
	var startTimer func()
	startTimer = func() {
		// Retire the previous source before arming the next, as before. Taking
		// the handle out of the struct first means the GLib call that retires
		// it runs with no lock held.
		if previous := iv.swapTimeout(0, time.Time{}); previous != 0 {
			glib.SourceRemove(previous)
		}

		// Read through the accessor: POST /api/interval writes this field from
		// an enqueued closure, so an unlocked read here would be a data race
		// against GET /api/state's reader.
		//
		// 🔴 ONE interval read, feeding BOTH the GLib source and the deadline
		// GET /api/state counts down from. Reading it twice — once for
		// TimeoutAdd and once for the deadline — would let POST /api/interval
		// land between them and produce a countdown for a duration no timer was
		// ever armed for. Same reason firesAt is computed BEFORE the TimeoutAdd
		// call rather than after it returns: this is the instant the source
		// starts counting from, and the two must not be separated by a cgo call.
		interval := iv.slideInterval()
		firesAt := time.Now().Add(time.Duration(interval) * time.Second)
		iv.swapTimeout(glib.TimeoutAdd(interval*1000, func() bool {
			if !iv.isPaused() {
				if iv.advance(iv.stepSize()) {
					iv.updateImage()
				}
			}
			startTimer()
			return false
		}), firesAt)
	}
	// Publish the arming closure BEFORE the first arm, so there is no window in
	// which a timer is armed and nothing can re-arm it. (main() calls this before
	// it starts the control API, so no request can land in that window anyway —
	// belt and braces, in the cheap direction.)
	//
	// 🔴 This is what POST /api/interval re-runs. It is deliberately the same
	// closure and not a copy: the retire-then-arm pair, the one interval read and
	// the one swapTimeout that writes the handle with its deadline are all in
	// here, and a second implementation of them is the two-clocks defect this
	// whole mechanism exists to prevent.
	iv.setArmTimer(startTimer)
	startTimer()
}

// installSignalHandler turns SIGINT/SIGTERM into a GTK main-loop quit, and a
// SECOND one into an immediate exit.
//
// 🔴 CORRECTED — the round-1 justification for the loop was FALSE, and it is
// recorded here rather than deleted because a maintainer who read it came away
// believing every `systemctl stop` was taking 90 s. It claimed that reading the
// channel once left the process unsignallable, so "systemctl stop then waits out
// TimeoutStopSec (90 s) on every stop and every restart". That could not happen:
// the read-once handler DID act on the first signal, and systemd sends exactly
// one SIGTERM and then SIGKILLs at the timeout — it never re-sends. One signal
// was always enough for the ordinary stop, before this loop and after it.
//
// What the loop actually buys, which is smaller but real: a SECOND signal can
// force an exit. Reading once and returning leaves signal.Notify still delivering
// into a channel nobody reads, so every later SIGINT/SIGTERM is buffered and
// dropped. That matters exactly when the graceful quit does NOT complete — the
// GTK loop still inside a 30 s S3 GET, or wedged — because the operator's second
// Ctrl-C or second `kill` is then the only thing short of SIGKILL that stops it.
// Under systemd it changes nothing about the normal stop; under a human at a
// terminal it is the difference between "press it again" and "find the PID".
//
// quit is SCHEDULED rather than called, so this returns immediately and the loop
// is always ready for the next signal. hardExit is os.Exit in production and is
// a parameter so the second-signal path is testable without killing the test
// binary.
func installSignalHandler(sigCh <-chan os.Signal, quit func(), hardExit func(int)) {
	go func() {
		quitting := false
		for sig := range sigCh {
			if !quitting {
				quitting = true
				log.Printf("received %s, quitting the GTK main loop", sig)
				// 🔴 Through scheduleQuit, NOT idleOnce — see its comment. The
				// priority is the difference between shutting down now and
				// shutting down after a backlog of 30 s image loads.
				scheduleQuit(quit)
				continue
			}
			log.Printf("received %s while already shutting down — exiting immediately", sig)
			hardExit(1)
			return
		}
	}()
}

// scheduleQuit puts quit on the GTK main loop AHEAD of queued work.
//
// 🔴 idleOnce would be wrong here, and silently so. It schedules at
// G_PRIORITY_DEFAULT_IDLE, which is where every control-API mutation closure
// sits — and updateSingleImage inside one of those can block for up to 30 s on
// an S3 GET. A quit queued behind a backlog of them waits for all of them.
// PRIORITY_HIGH jumps that queue, so shutdown waits only for the closure that
// is already RUNNING.
//
// It takes quit as a parameter so TestQuitJumpsTheQueueOfPendingWork can drive
// THIS function — the one that makes the priority decision — with a harmless
// payload instead of gtk.MainQuit. A test that called idleHigh directly would
// prove that idleHigh is high-priority and nothing about the shutdown path;
// that version of this guard let `idleHigh` -> `idleOnce` here survive.
func scheduleQuit(quit func()) { idleHigh(quit) }

func main() {
	// 🔴 The relative literal is load-bearing: the systemd unit's
	// WorkingDirectory=/home/zach/comic-flex is what makes it resolve. Do not
	// "clean this up" into an absolute path without changing the unit too.
	config, err := loadConfig("config.yaml")
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	store, err := NewImageStore(config)
	if err != nil {
		log.Fatalf("Failed to create image store: %v", err)
	}

	gtk.Init(nil)

	viewer, err := NewImageViewer(config, store)
	if err != nil {
		log.Fatalf("Failed to create viewer: %v", err)
	}
	// Startup does not rotate the display, matching previous behaviour: the
	// orientation the X session already has is taken as correct.
	_ = viewer.setViewModeState(parseViewMode(config.ViewMode))

	if err := viewer.setupUI(); err != nil {
		log.Fatalf("Failed to setup UI: %v", err)
	}

	// Start scanning for images in the background
	viewer.scanImagesAsync()

	// Start the slideshow
	viewer.startSlideshow()

	// The control API runs in this same process — it is not a second unit, so
	// there is no new supervision object and the existing unit restarts both
	// together. It returns nil, having logged why, if the token precondition
	// fails; the slideshow then runs with no control surface at all.
	ctrl := startControlAPI(viewer, controlAddr)

	// There was no signal handling here at all. gtk.MainQuit() is what returns
	// from gtk.Main(), so a SIGTERM has to be turned into one — and it must be
	// raised ON the GTK thread, which is what scheduleQuit is for.
	//
	// 🔴 Intercepting a signal is not free: before this existed, SIGTERM killed
	// the process instantly. Both properties below exist so that intercepting it
	// does not make `systemctl stop` WORSE than the default it replaced. See
	// installSignalHandler and scheduleQuit.
	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	installSignalHandler(sigCh, gtk.MainQuit, os.Exit)

	gtk.Main()

	// gtk.Main() has returned, so main() is about to end. This is the whole of
	// the new plumbing §4.4 calls for: without it the process would exit with
	// the listener still accepting.
	if ctrl != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := ctrl.Shutdown(ctx); err != nil {
			log.Printf("control API shutdown: %v", err)
		}
	}
}
