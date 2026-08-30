package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

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

func (s *S3Store) ListImages() ([]string, error) {
	ctx := context.Background()
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
	// scanning is true while a bucket listing is in flight. It is what lets
	// GET /api/state tell "not yet scanned" (total 0, indexing) apart from
	// "scanned and empty" (total 0, no comics). Guarded by mutex like the rest.
	scanning bool
}

// version is reported by GET /healthz.
const version = "0.2.0"

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

func (iv *ImageViewer) scanImagesAsync() {
	// Set synchronously, before the goroutine starts. Setting it inside the
	// goroutine would leave a window in which GET /api/state reports
	// scanning:false with total 0 — the "no comics" answer — for a gallery that
	// is about to be listed.
	iv.setScanning(true)

	go func() {
		defer iv.setScanning(false)

		images, err := iv.store.ListImages()
		if err != nil {
			log.Printf("Error listing images: %v", err)
			return
		}

		if iv.config.IsRandomOrder {
			rdm := rand.New(rand.NewSource(time.Now().UnixNano()))
			rdm.Shuffle(len(images), func(i, j int) {
				images[i], images[j] = images[j], images[i]
			})
		}

		iv.setImages(images)

		// Update the first image if none is showing. idleOnce is the program's
		// single glib.IdleAdd call site — see control_adapter.go.
		idleOnce(func() {
			iv.onScanComplete(iv.updateImage)
		})
	}()
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

	width, height := iv.window.GetSize()

	scaledPixbuf, err := scaleToFit(pixbuf, width, height)
	if err != nil {
		log.Printf("Unable to scale pixbuf: %v", err)
		return
	}

	iv.image.SetFromPixbuf(scaledPixbuf)
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

	winWidth, winHeight := iv.window.GetSize()

	// Layout: [margin][left image][gap][right image][margin]
	// Use consistent margins and gap for even spacing
	margin := 20
	gap := 40
	availableWidth := winWidth - (2 * margin) - gap
	maxImageWidth := availableWidth / 2

	// Scale both images to fit within their allocated space
	// Never crop - always scale to fit entirely within bounds
	leftScaled, err := scaleToFit(leftPb, maxImageWidth, winHeight)
	if err != nil {
		log.Printf("Unable to scale left image: %v", err)
		return
	}
	rightScaled, err := scaleToFit(rightPb, maxImageWidth, winHeight)
	if err != nil {
		log.Printf("Unable to scale right image: %v", err)
		return
	}

	// Get actual scaled dimensions
	lw := leftScaled.GetWidth()
	lh := leftScaled.GetHeight()
	rw := rightScaled.GetWidth()
	rh := rightScaled.GetHeight()

	// Create black canvas
	canvas, err := gdk.PixbufNew(gdk.COLORSPACE_RGB, true, 8, winWidth, winHeight)
	if err != nil {
		log.Printf("Unable to create canvas: %v", err)
		return
	}
	canvas.Fill(0x000000FF)

	// Position images with even spacing from center
	// Center point is at winWidth/2, gap is centered there
	centerX := winWidth / 2

	// Left image: right edge at centerX - gap/2
	leftX := centerX - gap/2 - lw
	leftY := (winHeight - lh) / 2

	// Right image: left edge at centerX + gap/2
	rightX := centerX + gap/2
	rightY := (winHeight - rh) / 2

	// Ensure positions are not negative (safety check)
	if leftX < 0 {
		leftX = 0
	}
	if leftY < 0 {
		leftY = 0
	}
	if rightY < 0 {
		rightY = 0
	}

	// Composite images onto canvas with bounds checking
	// dest_x/y: where to start writing in dest
	// dest_width/height: how many pixels to write (clamped to canvas bounds)
	// offset_x/y: maps source (0,0) to this dest coordinate

	// Left image - clamp dimensions to canvas bounds
	leftDestW := lw
	if leftX+lw > winWidth {
		leftDestW = winWidth - leftX
	}
	leftDestH := lh
	if leftY+lh > winHeight {
		leftDestH = winHeight - leftY
	}
	if leftDestW > 0 && leftDestH > 0 {
		leftScaled.Composite(canvas, leftX, leftY, leftDestW, leftDestH,
			float64(leftX), float64(leftY), 1.0, 1.0, gdk.INTERP_BILINEAR, 255)
	}

	// Right image - clamp dimensions to canvas bounds
	rightDestW := rw
	if rightX+rw > winWidth {
		rightDestW = winWidth - rightX
	}
	rightDestH := rh
	if rightY+rh > winHeight {
		rightDestH = winHeight - rightY
	}
	if rightDestW > 0 && rightDestH > 0 {
		rightScaled.Composite(canvas, rightX, rightY, rightDestW, rightDestH,
			float64(rightX), float64(rightY), 1.0, 1.0, gdk.INTERP_BILINEAR, 255)
	}

	iv.image.SetFromPixbuf(canvas)
	runtime.GC()
}

func scaleToFit(pb *gdk.Pixbuf, maxWidth, maxHeight int) (*gdk.Pixbuf, error) {
	origW := float64(pb.GetWidth())
	origH := float64(pb.GetHeight())
	scale := math.Min(float64(maxWidth)/origW, float64(maxHeight)/origH)
	destW := int(origW * scale)
	destH := int(origH * scale)
	return pb.ScaleSimple(destW, destH, gdk.INTERP_BILINEAR)
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
		if previous := iv.swapTimeout(0); previous != 0 {
			glib.SourceRemove(previous)
		}

		// Read through the accessor: POST /api/interval writes this field from
		// an enqueued closure, so an unlocked read here would be a data race
		// against GET /api/state's reader.
		iv.swapTimeout(glib.TimeoutAdd(iv.slideInterval()*1000, func() bool {
			if !iv.isPaused() {
				if iv.advance(iv.stepSize()) {
					iv.updateImage()
				}
			}
			startTimer()
			return false
		}))
	}
	startTimer()
}

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
	ctrl := startControlAPI(viewer)

	// There was no signal handling here at all. gtk.MainQuit() is what returns
	// from gtk.Main(), so a SIGTERM has to be turned into one — and it must be
	// raised ON the GTK thread, which is what idleOnce is for.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Printf("received %s, shutting down", sig)
		idleOnce(gtk.MainQuit)
	}()

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
