package main

import (
	"fmt"
	"log"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"time"

	"github.com/gotk3/gotk3/cairo"
	"github.com/gotk3/gotk3/gdk"
	"github.com/gotk3/gotk3/glib"
	"github.com/gotk3/gotk3/gtk"
	"github.com/gotk3/gotk3/pango"
	"gopkg.in/yaml.v2"
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

type Config struct {
	ContentDirectory string `yaml:"content_directory"`
	ManifestPath     string `yaml:"manifest_path"`
	SlideInterval    uint   `yaml:"slide_interval"`
	FillColor        string `yaml:"fill_color"`
	TextColor        string `yaml:"text_color"`
	EnableText       bool   `yaml:"enable_text"`
	IsRandomOrder    bool   `yaml:"is_random_order"`
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

	return &config, nil
}

func listImagesAsync(root string) (<-chan string, <-chan error) {
	imagesChan := make(chan string)
	errChan := make(chan error, 1) // Buffer of 1 to prevent blocking

	go func() {
		defer close(imagesChan)
		defer close(errChan)

		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if !info.IsDir() {
				switch filepath.Ext(path) {
				case ".jpg", ".jpeg", ".png", ".gif", ".bmp":
					imagesChan <- path
				}
			}
			return nil
		})

		if err != nil {
			errChan <- err
		}
	}()

	return imagesChan, errChan
}

func listImages(path string, isRandomOrder bool) ([]string, error) {
	imagesChan, errChan := listImagesAsync(path)
	var images []string

	for {
		select {
		case path, ok := <-imagesChan:
			if !ok {
				imagesChan = nil
			} else {
				// Append each image path to the slice
				images = append(images, path)
			}
		case err := <-errChan:
			if err != nil {
				log.Fatalf("Error listing images: %v", err)
			}
			errChan = nil
		}

		// Break out of the loop when both channels are closed
		if imagesChan == nil && errChan == nil {
			break
		}
	}

	if isRandomOrder {
		rdm := rand.New(rand.NewSource(time.Now().UnixNano()))
		rdm.Shuffle(len(images), func(i, j int) {
			images[i], images[j] = images[j], images[i]
		})
	}

	return images, nil
}

func hexToRGB(hexColor string) (float64, float64, float64, error) {
	var r, g, b uint8
	_, err := fmt.Sscanf(hexColor, "#%02x%02x%02x", &r, &g, &b)
	if err != nil {
		return 0, 0, 0, err
	}
	return float64(r) / 255.0, float64(g) / 255.0, float64(b) / 255.0, nil
}

func main() {
	config, err := loadConfig("config.yaml")
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	contentDirectory := config.ContentDirectory
	if contentDirectory == "" {
		contentDirectory = "./content"
	}

	slideInterval := config.SlideInterval * 1000
	if slideInterval == 0 {
		slideInterval = 30000
	}

	fillColor := config.FillColor
	if fillColor == "" {
		fillColor = "#ADD8E6"
	}

	enableText := config.EnableText

	textColor := config.TextColor
	if textColor == "" {
		textColor = "#000000"
	}

	fillColorR, fillColorG, fillColorB, err := hexToRGB(fillColor)
	if err != nil {
		log.Fatal(err)
	}

	manifestPath := config.ManifestPath
	if manifestPath == "" {
		manifestPath = "./manifest.yaml"
	}

	manifest, err := loadManifest(manifestPath)
	if err != nil {
		log.Fatalf("Failed to load manifest: %v", err)
	}

	gtk.Init(nil)

	win, err := gtk.WindowNew(gtk.WINDOW_TOPLEVEL)
	if err != nil {
		log.Fatal("Unable to create window:", err)
	}
	win.SetTitle("CAM COMIC FLEX")
	win.SetDefaultSize(1920, 1080)

	css := `
  window { background-color: black; }
`

	cssProvider, err := gtk.CssProviderNew()
	if err != nil {
		log.Fatal("Unable to create CSS provider:", err)
	}
	cssProvider.LoadFromData(css)

	screen, err := gdk.ScreenGetDefault()
	if err != nil {
		log.Fatal("Unable to get screen:", err)
	}

	gtk.AddProviderForScreen(screen, cssProvider, uint(gtk.STYLE_PROVIDER_PRIORITY_USER))

	win.Connect("destroy", func() {
		gtk.MainQuit()
	})
	win.Fullscreen()

	titleLabel, err := gtk.LabelNew("")
	if err != nil {
		log.Fatal("Unable to create label:", err)
	}

	titleLabel.SetHAlign(gtk.ALIGN_CENTER)
	titleLabel.SetHExpand(true)

	descLabel, err := gtk.LabelNew("")
	if err != nil {
		log.Fatal("Unable to create label:", err)
	}

	descLabel.SetLineWrap(true)
	descLabel.SetLineWrapMode(pango.WRAP_WORD) // Wrap at word boundaries
	descLabel.SetJustify(gtk.JUSTIFY_FILL)

	overlay, err := gtk.OverlayNew()
	if err != nil {
		log.Fatal("Unable to create overlay:", err)
	}

	img, err := gtk.ImageNew()
	if err != nil {
		log.Fatal("Unable to create image:", err)
	}
	overlay.Add(img)

	drawingArea, err := gtk.DrawingAreaNew()
	if err != nil {
		log.Fatal("Unable to create drawing area:", err)
	}
	drawingArea.SetSizeRequest(800, 100) // Set the size as per your requirement

	textCardHeight := 150.0

	// Draw event for drawing background
	drawingArea.Connect("draw", func(da *gtk.DrawingArea, cr *cairo.Context) {
		// Set the color for your background
		cr.SetSourceRGB(fillColorR, fillColorG, fillColorB)
		cr.Rectangle(0, float64(da.GetAllocatedHeight())-textCardHeight, float64(da.GetAllocatedWidth()), textCardHeight)
		cr.Fill()
	})
	overlay.AddOverlay(drawingArea)

	textContainer, err := gtk.BoxNew(gtk.ORIENTATION_VERTICAL, 0)
	if err != nil {
		log.Fatal("Unable to create text container:", err)
	}
	textContainer.SetVAlign(gtk.ALIGN_END) // Align at the bottom

	overlay.AddOverlay(textContainer)

	textContainer.PackStart(titleLabel, false, false, 10)
	textContainer.PackStart(descLabel, false, false, 10)

	win.Add(overlay)

	images, err := listImages(contentDirectory, config.IsRandomOrder)
	if err != nil {
		log.Fatalf("Failed to list images: %v", err)
	}

	currentIndex := 0

	// Function to update the image and reset timer
	var updateImage func() func()
	updateImage = func() func() {
		if currentIndex < 0 || currentIndex >= len(images) {
			currentIndex = 0
		}

		imagePath := images[currentIndex]

		fmt.Printf("%+v\n", imagePath)
		pixbuf, err := gdk.PixbufNewFromFile(imagePath)
		if err != nil {
			fmt.Printf("Unable to create pixbuf: %+v", err)
			return func() {
				gdk.Pixbuf.Unref(*pixbuf)
			}
		}

		if pixbuf == nil {
			fmt.Println("Pixbuf is nil")
			return func() {
			}
		}

		// Calculate the scale preserving aspect ratio
		origWidth := pixbuf.GetWidth()
		origHeight := pixbuf.GetHeight()

		if origWidth == 0 || origHeight == 0 {
			fmt.Println("Pixbuf width or height is 0")
			return func() {
				gdk.Pixbuf.Unref(*pixbuf)
			}
		}

		// Get window size
		width, height := win.GetSize()
		height = height - int(textCardHeight)

		scale := math.Min(float64(width)/float64(origWidth), float64(height)/float64(origHeight))

		// Scale the image
		destWidth := int(float64(origWidth) * scale)
		destHeight := int(float64(origHeight) * scale)
		fmt.Printf("%+v %v %v\n", pixbuf, destWidth, destHeight)
		scaledPixbuf, err := pixbuf.ScaleSimple(destWidth, destHeight, gdk.INTERP_BILINEAR)
		if err != nil {
			log.Fatal("Unable to scale pixbuf:", err)
			return func() {
				gdk.Pixbuf.Unref(*pixbuf)
				gdk.Pixbuf.Unref(*scaledPixbuf)
			}
		}

		img.Clear()
		img.SetFromPixbuf(scaledPixbuf)

		img.SetVAlign(gtk.ALIGN_START)

		if enableText {
			titleLabel.SetMarkup("")
			descLabel.SetMarkup("")
			overlay.Remove(drawingArea)
			overlay.Remove(textContainer)

			for _, entry := range manifest.Entries {
				if entry.ImagePath == imagePath {
					titleLabel.SetMarkup("<span foreground=\"" + textColor + "\" font=\"24\">" + entry.Title + "</span>")
					descLabel.SetMarkup("<span foreground=\"" + textColor + "\" font=\"20\">" + entry.Description + "</span>")
					overlay.AddOverlay(drawingArea)
					overlay.AddOverlay(textContainer)
					break
				}
			}
		}

		return func() {
			gdk.Pixbuf.Unref(*pixbuf)
			gdk.Pixbuf.Unref(*scaledPixbuf)
		}
	}

	var timeoutID glib.SourceHandle

	// Remove existing timeout and add a new one
	if timeoutID != 0 {
		glib.SourceRemove(timeoutID)
	}
	timeoutID = glib.TimeoutAdd(slideInterval, func() bool {
		currentIndex = (currentIndex + 1) % len(images)
		cleanup := updateImage()
		cleanup()
		return false // Stop the current timeout
	})

	// Key press event handler
	win.Connect("key-press-event", func(win *gtk.Window, event *gdk.Event) {
		keyEvent := &gdk.EventKey{Event: event}
		switch keyEvent.KeyVal() {
		case gdk.KEY_space, gdk.KEY_Right:
			currentIndex = (currentIndex + 1) % len(images)
		case gdk.KEY_Left:
			// Ensuring currentIndex doesn't go below 0
			if currentIndex == 0 {
				currentIndex = len(images) - 1
			} else {
				currentIndex--
			}
		}

		cleanup := updateImage()
		cleanup()
	})

	// Mouse click event handler
	win.Connect("button-press-event", func(win *gtk.Window, event *gdk.Event) {
		currentIndex = (currentIndex + 1) % len(images)
		cleanup := updateImage()
		cleanup()
	})

	// Initial image update
	cleanup := updateImage()
	cleanup()

	win.ShowAll()
	gtk.Main()
}
