package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"runtime"
	"sync"

	"github.com/ushu/udemy-backup/backup"
	"github.com/ushu/udemy-backup/client"
	"github.com/ushu/udemy-backup/client/lister"
	pb "gopkg.in/cheggaaa/pb.v1"
)

// Version of the tool
var Version = "0.4.1"

// Help message (before options)
const usageDescription = `Usage: udemy-backup

Make backups of Udemy course contents for offline usage.

OPTIONS:
`

// Flag values
var (
	showHelp    bool
	showVersion bool
	downloadAll bool
	quiet       bool
	redownload  bool
	output      string
	clientID    string
	accessToken string
)

// Number of parallel workers
var concurrency int

func init() {
	flag.BoolVar(&downloadAll, "a", false, "download all the courses enrolled by the user")
	flag.BoolVar(&showHelp, "h", false, "show usage info")
	flag.StringVar(&output, "o", ".", "output directory")
	flag.BoolVar(&quiet, "q", false, "disable output messages")
	flag.BoolVar(&redownload, "r", false, "force re-download of existing files")
	flag.BoolVar(&showVersion, "v", false, "show version number")
	flag.StringVar(&clientID, "c", "", "the client ID")
	flag.StringVar(&accessToken, "t", "", "the Access Token")
	flag.Usage = func() {
		fmt.Print(usageDescription)
		flag.PrintDefaults()
	}
	log.SetFlags(0)
	log.SetPrefix("")
	concurrency = runtime.GOMAXPROCS(0)
	if concurrency > 8 {
		concurrency = 8
	}
}

func main() {
	flag.Parse()
	ctx := context.Background()

	// Parse flags
	if showHelp {
		flag.Usage()
		return
	}
	if showVersion {
		fmt.Printf("v%s\n", Version)
		return
	}
	if quiet {
		log.SetOutput(ioutil.Discard)
	}

	// Connect to the Udemy backend
	c := client.New()
	if clientID == "" || accessToken == "" {
		// log the user in
		e, p, err := askCredentials()
		if err != nil {
			log.Fatal(err)
		}
		_, err = c.Login(ctx, e, p)
		if err != nil {
			log.Fatal(err)
		}
	} else {
		c.Credentials.ID = clientID
		c.Credentials.AccessToken = accessToken
	}

	// list all the courses
	l := lister.New(c)
	courses, err := l.ListAllCourses(ctx)
	if err != nil {
		log.Fatal(err)
	}

	// we're logged in !
	if downloadAll {
		for _, course := range courses {
			log.Printf("🚀 %s", course.Title)
			if err = downloadCourse(ctx, c, course); err != nil {
				log.Fatal(err)
			}
		}
	} else {
		course, err := selectCourse(courses)
		if err != nil {
			log.Fatal(err)
		}
		if err = downloadCourse(ctx, c, course); err != nil {
			log.Fatal(err)
		}
	}
}

func downloadCourse(ctx context.Context, client *client.Client, course *client.Course) error {
	var err error

	// list all the available course elements
	b := backup.New(client, output, false)
	allAssets, dirs, err := b.ListCourseAssets(ctx, course)
	if err != nil {
		return err
	}

	// create all the required directories
	for _, d := range dirs {
		if !dirExists(d) {
			if err = os.MkdirAll(d, 0755); err != nil {
				log.Fatal(err)
			}
		}
	}

	// filter already-downloaded assets when "redownload" is selected
	var assets []backup.Asset
	if !redownload {
		for _, a := range allAssets {
			if !fileExists(a.LocalPath) {
				assets = append(assets, a)
			}
		}
	} else {
		assets = allAssets
	}

	// create the "bar"
	var bar *pb.ProgressBar
	if !quiet {
		bar = pb.New(len(allAssets))
		bar.Add(len(allAssets) - len(assets))
		bar.Start()
		defer bar.Update()
	}

	// start a cancelable context
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// we use a pull of workers
	chwork := make(chan backup.Asset)      // assets to process get enqueued here
	cherr := make(chan error, concurrency) // download results

	// start the workers
	var wg sync.WaitGroup
	wg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			defer wg.Done()
			for a := range chwork {
				if a.RemoteURL != "" {
					var err error
				Retries:
					for retry := 0; retry < 3; retry++ {
						err = downloadURLToFile(ctx, client.HTTPClient, a.RemoteURL, a.LocalPath)
						if err == nil {
							break Retries
						}
					}
					cherr <- err
				} else if len(a.Contents) > 0 {
					cherr <- ioutil.WriteFile(a.LocalPath, a.Contents, os.ModePerm)
				}
				if !quiet {
					bar.Increment()
				}
			}
		}()
	}

	// and the "pusher" goroutine
	go func() {
		// enqueue all assets (unless we cancel)
		for _, a := range assets {
			select {
			case <-ctx.Done():
				break
			case chwork <- a:
			}
		}
		// we close channels on "enqueing" side to avoid panics
		close(chwork) // <- will stop the workers
		wg.Wait()
		close(cherr) // <- we close when we are sure there won't be a "write"
	}()

	// we wait for an error (if any)
	for err := range cherr {
		if err != nil {
			return err // <- will cancel the context, then the "pusher", then the workers
		}
	}
	return nil
}

func downloadURLToFile(ctx context.Context, c *http.Client, url, filePath string) error {
	tmpPath := filePath + ".tmp"

	// open file for writing
	f, err := os.Create(tmpPath)
	if err != nil {
		return err
	}

	// connect to the backend to get the file
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		_ = f.Close()
		return err
	}
	req = req.WithContext(ctx)
	res, err := c.Do(req)
	if err != nil {
		_ = f.Close()
		return err
	}

	// load all the data into the local file
	_, err = io.Copy(f, res.Body)
	_ = res.Body.Close()
	if err != nil {
		_ = f.Close()
		return err
	}

	// finally move the temp file into the final place
	err = f.Close()
	if err != nil {
		return err
	}
	return os.Rename(tmpPath, filePath)
}

func fileExists(name string) bool {
	_, err := os.Stat(name)
	return !os.IsNotExist(err)
}

func dirExists(name string) bool {
	s, err := os.Stat(name)
	return !os.IsNotExist(err) && s.IsDir()
}
