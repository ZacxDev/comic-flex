package main

import (
	"database/sql"
	"encoding/gob"
	"fmt"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	gocollect "github.com/ZacxDev/go-gocollect-sdk"
	_ "github.com/mattn/go-sqlite3"
	"github.com/pkg/errors"
)

// Comic represents a record from the SQLite database
type Comic struct {
	ID                  int
	FilePath            string
	Series              string
	IssueNumber         int
	ReleaseDate         string
	Publisher           string
	CGCCertificationNum string
	CGCGrade            string
	ProcessedAt         time.Time
	GocollectID         *int
}

// CachedInsights represents cached insight data with expiration
type CachedInsights struct {
	Insights  *gocollect.ItemInsights
	ExpiresAt time.Time
}

// InsightsCache handles caching of GoCollect API responses
type InsightsCache struct {
	cache     map[string]CachedInsights
	mutex     sync.RWMutex
	client    *gocollect.Client
	cacheFile string
}

// NewInsightsCache creates a new cache instance
func NewInsightsCache(apiToken, cacheFile string) (*InsightsCache, error) {
	client, err := gocollect.NewClient(apiToken)
	if err != nil {
		return nil, fmt.Errorf("failed to create GoCollect client: %v", err)
	}

	cache := &InsightsCache{
		cache:     make(map[string]CachedInsights),
		client:    client,
		cacheFile: cacheFile,
	}

	// Load cached data from file if it exists
	if err := cache.loadCache(); err != nil {
		return nil, fmt.Errorf("failed to load cache: %v", err)
	}

	return cache, nil
}

// GetInsights retrieves insights either from cache or API
func (ic *InsightsCache) GetInsights(comic *Comic, grade string) (*gocollect.ItemInsights, int, error) {
	var insights *gocollect.ItemInsights
	var gocollectID int
	if comic.GocollectID != nil {
		gocollectID = *comic.GocollectID
	}

	if comic.CGCCertificationNum != "" && comic.CGCCertificationNum != "0" {
		fmt.Printf("using CGC cert num %v\n", comic.CGCCertificationNum)
		ic.mutex.RLock()
		if cached, ok := ic.cache[comic.CGCCertificationNum]; ok && time.Now().Before(cached.ExpiresAt) {
			ic.mutex.RUnlock()
			fmt.Printf("cache hit\n")
			return cached.Insights, gocollectID, nil
		}
		ic.mutex.RUnlock()

		// Not in cache or expired, fetch from API
		insights, err := ic.client.Insights.GetItemInsightsByCGCID(comic.CGCCertificationNum, comic.CGCGrade, "CGC", "")
		if err != nil {
			return nil, gocollectID, fmt.Errorf("failed to get insights from API: %v", err)
		}

		// Cache the result
		ic.mutex.Lock()
		ic.cache[comic.CGCCertificationNum] = CachedInsights{
			Insights:  insights,
			ExpiresAt: time.Now().Add(30 * 24 * time.Hour), // 30 day cache
		}
		ic.mutex.Unlock()
	} else if comic.GocollectID != nil && *comic.GocollectID != 0 {
		gocollectID = *comic.GocollectID
		fmt.Printf("using go collect id %v and grade %v\n", gocollectID, grade)
		key := strconv.Itoa(gocollectID) + grade

		ic.mutex.RLock()
		if cached, ok := ic.cache[key]; ok && time.Now().Before(cached.ExpiresAt) {
			fmt.Printf("cache hit\n")
			ic.mutex.RUnlock()
			return cached.Insights, gocollectID, nil
		}
		ic.mutex.RUnlock()

		insights, err := ic.client.Insights.GetItemInsights(gocollectID, grade, "", "")
		if err != nil {
			return nil, gocollectID, fmt.Errorf("failed to get insights from API: %v", err)
		}

		// Cache the result
		ic.mutex.Lock()
		ic.cache[key] = CachedInsights{
			Insights:  insights,
			ExpiresAt: time.Now().Add(30 * 24 * time.Hour), // 30 day cache
		}
		ic.mutex.Unlock()
	} else {
		fmt.Printf("falling back to search endpoint\n")
		seriesWithNumber := comic.Series + strconv.Itoa(comic.IssueNumber)

		ic.mutex.RLock()
		if cached, ok := ic.cache[seriesWithNumber]; ok && time.Now().Before(cached.ExpiresAt) {
			ic.mutex.RUnlock()
			fmt.Printf("cache hit\n")
			return cached.Insights, gocollectID, nil
		}
		ic.mutex.RUnlock()

		query := comic.Series + "#" + strconv.Itoa(comic.IssueNumber)
		fmt.Printf("searching %v in grade %v\n", query, grade)

		searchRes, err := ic.client.Collectibles.SearchItems(gocollect.SearchItemsOptions{
			Query: query,
			Limit: 1,
			CAM:   "Comics",
		})
		if err != nil {
			return nil, gocollectID, fmt.Errorf("failed to get insights from API: %v", err)
		}

		if len(searchRes) > 0 {
			gocollectID = searchRes[0].ItemID
			key := strconv.Itoa(gocollectID) + grade
			insights, err := ic.client.Insights.GetItemInsights(gocollectID, grade, "", "")
			if err != nil {
				return nil, gocollectID, fmt.Errorf("failed to get insights from API: %v", err)
			}
			fmt.Printf("got gocollectID from search: %+v\n", gocollectID)

			// Cache the result
			ic.mutex.Lock()
			ic.cache[key] = CachedInsights{
				Insights:  insights,
				ExpiresAt: time.Now().Add(30 * 24 * time.Hour), // 30 day cache
			}
			ic.mutex.Unlock()
		} else {
			fmt.Printf("no search results for: \"%s\"", query)
		}
	}

	// Save cache to file
	if err := ic.saveCache(); err != nil {
		return nil, gocollectID, fmt.Errorf("failed to save cache: %v", err)
	}

	return insights, gocollectID, nil
}

// loadCache loads cached data from file
func (ic *InsightsCache) loadCache() error {
	file, err := os.Open(ic.cacheFile)
	if os.IsNotExist(err) {
		return nil // No cache file yet
	}
	if err != nil {
		return err
	}
	defer file.Close()

	decoder := gob.NewDecoder(file)
	ic.mutex.Lock()
	defer ic.mutex.Unlock()
	return decoder.Decode(&ic.cache)
}

// saveCache saves cached data to file
func (ic *InsightsCache) saveCache() error {
	// Ensure directory exists
	dir := filepath.Dir(ic.cacheFile)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	file, err := os.Create(ic.cacheFile)
	if err != nil {
		return err
	}
	defer file.Close()

	ic.mutex.RLock()
	defer ic.mutex.RUnlock()
	encoder := gob.NewEncoder(file)
	return encoder.Encode(ic.cache)
}

// ComicService handles comic database operations
type ComicService struct {
	db    *sql.DB
	cache *InsightsCache
}

// NewComicService creates a new comic service instance
func NewComicService(dbPath, apiToken string) (*ComicService, error) {
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %v", err)
	}

	cache, err := NewInsightsCache(apiToken, "cache/insights.gob")
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to create insights cache: %v", err)
	}

	return &ComicService{
		db:    db,
		cache: cache,
	}, nil
}

// Close closes the database connection
func (cs *ComicService) Close() error {
	return cs.db.Close()
}

// GetRandomComic retrieves a random comic from the database and its insights
func (cs *ComicService) GetRandomComic() (*Comic, *gocollect.ItemInsights, error) {
	// Get total count of comics
	var count int
	err := cs.db.QueryRow("SELECT COUNT(*) FROM comics").Scan(&count)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get comic count: %v", err)
	}

	// Get random offset
	offset := rand.Intn(count)

	// Get random comic
	comic := &Comic{}
	err = cs.db.QueryRow(`
		SELECT id, file_path, series, issue_number, release_date, 
		       publisher, cgc_certification_number, cgc_grade, processed_at, gocollect_id 
		FROM comics LIMIT 1 OFFSET ?`, offset).Scan(
		&comic.ID, &comic.FilePath, &comic.Series, &comic.IssueNumber,
		&comic.ReleaseDate, &comic.Publisher, &comic.CGCCertificationNum,
		&comic.CGCGrade, &comic.ProcessedAt, &comic.GocollectID)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get random comic: %v", err)
	}

	// Get insights
	fmt.Printf("looking up comic series: %s, #%v, CGC num: %v\n", comic.Series, comic.IssueNumber, comic.CGCCertificationNum)
	grades := []string{"9.8", "9.4", "9.0"}
	var insights *gocollect.ItemInsights
	var gocollectID int

	for _, grade := range grades {
		insights, gocollectID, err = cs.cache.GetInsights(comic, grade)
		if err != nil {
			return comic, insights, errors.WithStack(err)
		}

		if insights != nil && insights.FMV != nil && *insights.FMV > 0 {
			break
		}
	}

	if insights == nil {
		fmt.Print("got nil insights\n")
	} else {
		fmt.Printf("got insight: %+v\n", insights)
	}
	// Update gocollect_id if we got a new one and it's different from what's stored
	if gocollectID != 0 && (comic.GocollectID == nil || *comic.GocollectID != gocollectID) {
		_, err = cs.db.Exec(`
                        UPDATE comics 
                        SET gocollect_id = ?
                        WHERE id = ?`,
			gocollectID, comic.ID)
		if err != nil {
			// Log the error but don't fail the whole operation
			log.Printf("Failed to update gocollect_id for comic %d: %v", comic.ID, err)
		} else {
			// Update the comic struct to reflect the change
			comic.GocollectID = &gocollectID
		}
	}

	return comic, insights, nil
}

func init() {
	// Register types for gob encoding
	gob.Register(map[string]gocollect.Metrics{})
	gob.Register(gocollect.ItemInsights{})
}
