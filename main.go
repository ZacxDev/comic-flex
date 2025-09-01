package main

import (
	"fmt"
	"log"
	"math"
	"os"
	"strconv"
	"sync"

	gocollect "github.com/ZacxDev/go-gocollect-sdk"
	"github.com/gotk3/gotk3/gdk"
	"github.com/gotk3/gotk3/glib"
	"github.com/gotk3/gotk3/gtk"
	"github.com/gotk3/gotk3/pango"
)

type ImageViewer struct {
	mutex           *sync.RWMutex
	window          *gtk.Window
	image           *gtk.Image
	titleLabel      *gtk.Label
	descLabel       *gtk.Label
	statsLabel      *gtk.Label
	comicService    *ComicService
	timeoutID       glib.SourceHandle
	currentComic    *Comic
	currentInsights *gocollect.ItemInsights
}

func hexToRGB(hexColor string) (float64, float64, float64, error) {
	var r, g, b uint8
	_, err := fmt.Sscanf(hexColor, "#%02x%02x%02x", &r, &g, &b)
	if err != nil {
		return 0, 0, 0, err
	}
	return float64(r) / 255.0, float64(g) / 255.0, float64(b) / 255.0, nil
}

func NewImageViewer(comicService *ComicService) (*ImageViewer, error) {
	win, err := gtk.WindowNew(gtk.WINDOW_TOPLEVEL)
	if err != nil {
		return nil, err
	}

	img, err := gtk.ImageNew()
	if err != nil {
		return nil, err
	}

	titleLabel, err := gtk.LabelNew("")
	if err != nil {
		return nil, err
	}

	descLabel, err := gtk.LabelNew("")
	if err != nil {
		return nil, err
	}

	statsLabel, err := gtk.LabelNew("")
	if err != nil {
		return nil, err
	}

	return &ImageViewer{
		mutex:        &sync.RWMutex{},
		window:       win,
		image:        img,
		titleLabel:   titleLabel,
		descLabel:    descLabel,
		statsLabel:   statsLabel,
		comicService: comicService,
	}, nil
}

func (iv *ImageViewer) updateImage() (error, func()) {
	comic, insights, err := iv.comicService.GetRandomComic()
	if err != nil {
		return fmt.Errorf("failed to get random comic: %v", err), func() {}
	}

	iv.mutex.Lock()
	iv.currentComic = comic
	iv.currentInsights = insights
	iv.mutex.Unlock()

	pixbuf, err := gdk.PixbufNewFromFile(comic.FilePath)
	if err != nil {
		return fmt.Errorf("unable to create pixbuf: %v", err), func() {}
	}

	width, height := iv.window.GetSize()

	if insights != nil && insights.FMV != nil && *insights.FMV > 0 {
		height = height - 200 // Adjust for text area height if we are showing insights data
	}

	origWidth := pixbuf.GetWidth()
	origHeight := pixbuf.GetHeight()
	scale := math.Min(float64(width)/float64(origWidth), float64(height)/float64(origHeight))

	destWidth := int(float64(origWidth) * scale)
	destHeight := int(float64(origHeight) * scale)

	scaledPixbuf, err := pixbuf.ScaleSimple(destWidth, destHeight, gdk.INTERP_BILINEAR)
	if err != nil {
		return fmt.Errorf("unable to scale pixbuf: %v", err), func() {}
	}

	iv.image.SetFromPixbuf(scaledPixbuf)

	// Update sales insights
	if insights != nil {
		titleText := ""
		statsText := "<span font=\"18\" foreground=\"white\">"
		if insights.FMV != nil {
			titleText = "<span font=\"18\" foreground=\"white\">"
			titleText += comic.Series + " #" + strconv.Itoa(comic.IssueNumber)
			titleText += "</span>"

			statsText += fmt.Sprintf("FMV (Grade of %v): $%.2f\n", insights.Grade, *insights.FMV)
		}

		// We'll check metrics in order: last_30_days, then last_60_days, then last_90_days
		var metrics *gocollect.Metrics
		var period string

		// Check last_30_days
		if m, ok := insights.Metrics["30"]; ok && m.AveragePrice != 0 {
			metrics = &m
			period = "Last 30 Days"
		} else if m, ok := insights.Metrics["60"]; ok && m.AveragePrice != 0 {
			// Otherwise, check last_60_days
			metrics = &m
			period = "Last 60 Days"
		} else if m, ok := insights.Metrics["365"]; ok && m.AveragePrice != 0 {
			// Otherwise, check last year
			metrics = &m
			period = "Last Year"
		}

		// If we found any metrics data for those periods
		if metrics != nil {
			statsText += fmt.Sprintf("%s:\n", period)
			statsText += fmt.Sprintf("Sales: %d\n", metrics.SoldCount)
			statsText += fmt.Sprintf("Average: $%.2f\n", metrics.AveragePrice)
			statsText += fmt.Sprintf("Range: $%.2f - $%.2f", metrics.LowPrice, metrics.HighPrice)
		}

		statsText += "</span>"
		iv.statsLabel.SetMarkup(statsText)
		iv.titleLabel.SetMarkup(titleText)
	}

	return nil, func() {
		pixbuf.Unref()
		scaledPixbuf.Unref()
		//runtime.GC()
	}
}

func (iv *ImageViewer) setupUI() error {
	iv.window.SetTitle("Comic Viewer")
	iv.window.SetDefaultSize(1920, 1080)

	css := `
                window { background-color: black; }
                span { color: white; }
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

	// Configure labels
	iv.titleLabel.SetHAlign(gtk.ALIGN_CENTER)
	iv.titleLabel.SetHExpand(true)

	iv.descLabel.SetLineWrap(true)
	iv.descLabel.SetLineWrapMode(pango.WRAP_WORD)
	iv.descLabel.SetJustify(gtk.JUSTIFY_CENTER)

	iv.statsLabel.SetLineWrap(true)
	iv.statsLabel.SetLineWrapMode(pango.WRAP_WORD)
	iv.statsLabel.SetJustify(gtk.JUSTIFY_CENTER)

	// Create overlay
	overlay, err := gtk.OverlayNew()
	if err != nil {
		return fmt.Errorf("unable to create overlay: %v", err)
	}

	// Add image to overlay
	iv.image.SetVAlign(gtk.ALIGN_START)
	overlay.Add(iv.image)

	textContainer, err := gtk.BoxNew(gtk.ORIENTATION_VERTICAL, 0)
	if err != nil {
		return fmt.Errorf("unable to create text container: %v", err)
	}
	textContainer.SetVAlign(gtk.ALIGN_END)

	textContainer.PackStart(iv.titleLabel, false, false, 10)
	textContainer.PackStart(iv.statsLabel, false, false, 10)

	overlay.AddOverlay(textContainer)

	// Setup event handlers
	iv.window.Connect("destroy", func() {
		gtk.MainQuit()
	})

	iv.window.Connect("key-press-event", func(win *gtk.Window, event *gdk.Event) {
		keyEvent := &gdk.EventKey{Event: event}
		switch keyEvent.KeyVal() {
		case gdk.KEY_space, gdk.KEY_Right:
			err, cleanup := iv.updateImage()
			defer cleanup()
			if err != nil {
				log.Fatal(err.Error())
			}

		}
	})

	iv.window.Connect("button-press-event", func(win *gtk.Window, event *gdk.Event) {
		err, cleanup := iv.updateImage()
		defer cleanup()
		if err != nil {
			log.Fatal(err.Error())
		}

	})

	iv.window.Add(overlay)
	iv.window.ShowAll()

	return nil
}

func (iv *ImageViewer) startSlideshow(interval uint) {
	var startTimer func()
	startTimer = func() {
		if iv.timeoutID != 0 {
			glib.SourceRemove(iv.timeoutID)
		}

		iv.timeoutID = glib.TimeoutAdd(interval*1000, func() bool {
			err, cleanup := iv.updateImage()
			defer cleanup()
			if err != nil {
				log.Fatal(err.Error())
			}

			startTimer()
			return false
		})
	}
	startTimer()
}

func main() {
	apiToken := os.Getenv("GOCOLLECT_API_TOKEN")
	if apiToken == "" {
		log.Fatal("GOCOLLECT_API_TOKEN environment variable is required")
	}

	comicService, err := NewComicService("comics.db", apiToken)
	if err != nil {
		log.Fatalf("Failed to create comic service: %v", err)
	}
	defer comicService.Close()

	gtk.Init(nil)

	viewer, err := NewImageViewer(comicService)
	if err != nil {
		log.Fatalf("Failed to create viewer: %v", err)
	}

	if err := viewer.setupUI(); err != nil {
		log.Fatalf("Failed to setup UI: %v", err)
	}

	// Load initial random comic
	err, cleanup := viewer.updateImage()
	defer cleanup()
	if err != nil {
		log.Printf("Failed to load initial image: %v", err)
	}

	// Start the slideshow (30 second intervals)
	viewer.startSlideshow(30)

	gtk.Main()
}
