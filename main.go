package main

import (
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
	"golang.org/x/sync/errgroup"
	"gopkg.in/natefinch/lumberjack.v2"
)

//go:embed kobodeck.toml
var configTemplate []byte

var (
	configFileFlag = flag.String("config", "", "path to the configuration file")
	checkFlag      = flag.Bool("check", false, "validate config and show what would be synced, then exit")
)

type appConfig struct {
	Server serverConfig `toml:"Server"`
	Fetch  fetchConfig  `toml:"Fetch"`
	Sync   syncConfig   `toml:"Sync"`
	Log    logConfig    `toml:"Log"`
	Output outputConfig `toml:"Output"`
}

type serverConfig struct {
	URL     string `toml:"URL"`
	Token   string `toml:"Token"`
	Timeout int    `toml:"Timeout"`
}

type fetchConfig struct {
	Workers int    `toml:"Workers"`
	Limit   int    `toml:"Limit"`
	Labels  string `toml:"Labels"`
	Status  string `toml:"Status"`
}

type syncConfig struct {
	Archive             bool   `toml:"Archive"`
	FavouriteCollection string `toml:"FavouriteCollection"`
}

type logConfig struct {
	Verbose bool `toml:"Verbose"`
	Size    int  `toml:"Size"` // in MB
}

type outputConfig struct {
	Path   string `toml:"Path"`
	Delete bool   `toml:"Delete"`
}

// validate checks that all required config fields are present and sane.
func (c *appConfig) validate() error {
	if c.Server.URL == "" {
		return fmt.Errorf("Server.URL is required")
	}
	serverURL, err := url.Parse(c.Server.URL)
	if err != nil || (serverURL.Scheme != "http" && serverURL.Scheme != "https") || serverURL.Host == "" || serverURL.RawQuery != "" || serverURL.Fragment != "" {
		return fmt.Errorf("Server.URL must be a valid http or https URL with a host and no query or fragment")
	}
	if c.Server.Token == "" {
		return fmt.Errorf("Server.Token is required")
	}
	if c.Fetch.Limit < 0 {
		return fmt.Errorf("Fetch.Limit must not be negative")
	}
	if c.Fetch.Workers > 32 {
		return fmt.Errorf("Fetch.Workers must not exceed 32")
	}
	for _, status := range strings.Split(c.Fetch.Status, ",") {
		status = strings.TrimSpace(status)
		if status != "" && status != "unread" && status != "reading" && status != "read" {
			return fmt.Errorf("Fetch.Status contains invalid value %q", status)
		}
	}
	if c.Log.Size < 0 {
		return fmt.Errorf("Log.Size must not be negative")
	}
	if c.Output.Path == "" {
		return fmt.Errorf("Output.Path is required")
	}
	cleanOutputPath := filepath.Clean(c.Output.Path)
	if !filepath.IsAbs(c.Output.Path) || cleanOutputPath == filepath.VolumeName(cleanOutputPath)+string(filepath.Separator) {
		return fmt.Errorf("Output.Path must be an absolute path other than the filesystem root")
	}
	if c.Fetch.Workers <= 0 {
		return fmt.Errorf("Fetch.Workers must be greater than 0")
	}
	if c.Server.Timeout <= 0 {
		return fmt.Errorf("Server.Timeout must be greater than 0")
	}
	return nil
}

var buildVersion = "dev"

type app struct {
	cfg              appConfig
	readeck          readeckClient
	nickel           nickelLibrary
	lockFilePath     string
	nickelStatusPath string
}

type downloadRun struct {
	group        errgroup.Group
	download     func(readeckBookmark) (bool, error)
	filesChanged atomic.Bool
	failures     chan error
}

func newDownloadRun(workerLimit, maxDownloads int, download func(readeckBookmark) (bool, error)) *downloadRun {
	run := &downloadRun{
		download: download,
		failures: make(chan error, maxDownloads),
	}
	run.group.SetLimit(workerLimit)
	return run
}

func (run *downloadRun) start(entry readeckBookmark) {
	run.group.Go(func() error {
		changed, err := run.download(entry)
		if changed {
			run.filesChanged.Store(true)
		}
		if err != nil {
			run.failures <- fmt.Errorf("bookmark %s: %w", entry.ID, err)
		}
		// Failures are collected above so the group only controls concurrency
		// and completion rather than discarding all but its first error.
		return nil
	})
}

// wait completes the download run and may only be called once.
func (run *downloadRun) wait() (bool, error) {
	waitErr := run.group.Wait()
	close(run.failures)
	var failures []error
	for err := range run.failures {
		failures = append(failures, err)
	}
	return run.filesChanged.Load(), errors.Join(errors.Join(failures...), waitErr)
}

func newApp(cfg appConfig) app {
	client := &http.Client{
		Timeout: time.Duration(cfg.Server.Timeout) * time.Second,
	}
	readeck := newReadeckClient(client, cfg.Server, cfg.Log.Verbose)
	return app{
		cfg:              cfg,
		readeck:          readeck,
		nickel:           nickelDatabase{path: defaultNickelDBPath, verbose: cfg.Log.Verbose},
		lockFilePath:     defaultLockFilePath,
		nickelStatusPath: defaultNickelStatusPath,
	}
}

func main() {
	flag.Parse()
	if err := os.MkdirAll(filepath.Dir(confPath), 0o755); err != nil {
		log.Fatal("create config directory: ", err)
	}
	configFile, cfg, configErr := findConfig()
	setupLogging(cfg, configFile)
	log.SetPrefix(fmt.Sprintf("pid=%d ", os.Getpid()))
	debug.SetPanicOnFault(true)

	if errors.Is(configErr, errConfigCreated) {
		log.Printf("no config found — template written to %s, please edit it", confPath)
		return
	} else if errors.Is(configErr, errUninstallRequested) {
		log.Println("empty config found — uninstalling")
		doUninstall(os.Args[0], installFiles)
		if err := os.RemoveAll(filepath.Dir(confPath)); err != nil {
			log.Fatal("remove application directory: ", err)
		}
		log.Println("uninstall complete")
		return
	} else if configErr != nil {
		log.Fatal("invalid configuration: ", configErr)
	}
	if err := cfg.validate(); err != nil {
		log.Fatal("invalid configuration: ", err)
	}
	log.Printf("kobodeck version %s loaded configuration from %s action=%q interface=%q",
		buildVersion, configFile, os.Getenv("ACTION"), os.Getenv("INTERFACE"))

	application := newApp(cfg)
	if *checkFlag {
		if err := application.runCheck(os.Stdout); err != nil {
			log.Fatal("check failed: ", err)
		}
		return
	}

	start := time.Now()
	defer func() {
		log.Printf("completed in %s", time.Since(start).Truncate(time.Millisecond))
	}()

	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)

	if err := application.sync(sigc); err != nil {
		log.Fatal(err)
	}
}

func (a app) sync(sigc <-chan os.Signal) error {
	lock, err := acquireLock(a.lockFilePath)
	if err != nil {
		return err
	}
	defer closeWithWarning("lock file", lock)

	log.Println("connecting to", a.cfg.Server.URL)
	time.Sleep(5 * time.Second)
	entries, err := a.readeck.listBookmarks(a.cfg.Fetch)
	for attempt := 1; err != nil && attempt < 5; attempt++ {
		delay := time.Duration(1<<uint(attempt)) * time.Second
		log.Printf("failed to connect, retrying in %s: %v", delay, err)
		time.Sleep(delay)
		entries, err = a.readeck.listBookmarks(a.cfg.Fetch)
	}
	if err != nil {
		return err
	}

	tags := make(map[string]bool)
	if len(a.cfg.Fetch.Labels) > 0 {
		for _, tag := range strings.Split(strings.ToLower(a.cfg.Fetch.Labels), ",") {
			tags[strings.TrimSpace(tag)] = true
		}
	}
	valid := make(map[string]bool)
	bookmarks := make(map[string]readeckBookmark, len(entries))
	for _, entry := range entries {
		bookmarks[entry.ID] = entry
		if len(tags) == 0 || matchesLabelFilter(tags, entry.Labels) {
			valid[entry.ID] = true
		}
	}

	downloads := newDownloadRun(a.cfg.Fetch.Workers, len(entries), func(entry readeckBookmark) (bool, error) {
		return a.readeck.downloadBookmarkFile(a.cfg.Output, entry)
	})
	filesChanged := false
	cancelled := false

	for _, entry := range entries {
		if !valid[entry.ID] {
			debugf(a.cfg.Log.Verbose, "skipping %s (not in tags)", entry.ID)
			continue
		}
		select {
		case sig := <-sigc:
			log.Println("got signal:", sig, ", waiting for downloads to finish...")
			cancelled = true
			goto done
		default:
		}
		debugf(a.cfg.Log.Verbose, "dispatching %s", entry.ID)
		downloads.start(entry)
	}
done:
	var syncErr error
	downloadsChanged, err := downloads.wait()
	if err != nil {
		log.Println("download error:", err)
		syncErr = errors.Join(syncErr, fmt.Errorf("downloads failed: %w", err))
	}
	if downloadsChanged {
		filesChanged = true
	}
	select {
	case sig := <-sigc:
		log.Println("got signal:", sig, ", downloads finished")
		cancelled = true
	default:
	}

	changed, err := reconcileLocalFiles(a.readeck, a.nickel, a.cfg, valid, bookmarks, a.cfg.Output.Delete && !cancelled)
	if err != nil {
		log.Println("reconciliation error:", err)
		syncErr = errors.Join(syncErr, err)
	}
	if changed {
		filesChanged = true
	}

	if filesChanged {
		if err := nickelRescan(a.nickelStatusPath); err != nil {
			log.Printf("nickel rescan failed: %v", err)
			syncErr = errors.Join(syncErr, fmt.Errorf("nickel rescan failed: %w", err))
		}
	}
	return syncErr
}

func debugf(verbose bool, format string, args ...interface{}) {
	if verbose {
		log.Printf(format, args...)
	}
}

const confPath = "/mnt/onboard/.adds/kobodeck/kobodeck.toml"

const (
	defaultNickelDBPath     = "/mnt/onboard/.kobo/KoboReader.sqlite"
	defaultNickelStatusPath = "/tmp/nickel-hardware-status"
	defaultLockFilePath     = "/tmp/kobodeck.lock"
)

var logPath = filepath.Join(filepath.Dir(confPath), "kobodeck.log")

// setupLogging configures the global logger to write to a size-capped rotating
// log file beside the resolved configuration file.
func setupLogging(cfg appConfig, configFilename string) {
	maxSizeMB := cfg.Log.Size
	if maxSizeMB < 1 {
		maxSizeMB = 1
	}
	filename := logPath
	if configFilename != "" {
		filename = filepath.Join(filepath.Dir(configFilename), "kobodeck.log")
	}
	log.SetOutput(&lumberjack.Logger{
		Filename:   filename,
		MaxSize:    maxSizeMB,
		MaxBackups: 7,
		MaxAge:     7,
	})
}

var errUninstallRequested = errors.New("uninstall requested")

// loadConfig decodes the TOML file at path.
// Returns os.ErrNotExist if the file is absent, errUninstallRequested if
// the file is empty, or an error for parse failures and unrecognised keys.
func loadConfig(path string) (_ appConfig, returnErr error) {
	f, err := os.Open(path)
	if err != nil {
		return appConfig{}, err
	}
	defer func() {
		returnErr = errors.Join(returnErr, f.Close())
	}()
	info, err := f.Stat()
	if err != nil {
		return appConfig{}, err
	}
	if info.Size() == 0 {
		return appConfig{}, errUninstallRequested
	}
	var cfg appConfig
	md, err := toml.NewDecoder(f).Decode(&cfg)
	if err != nil {
		return appConfig{}, err
	}
	if keys := md.Undecoded(); len(keys) > 0 {
		return appConfig{}, fmt.Errorf("unknown keys: %v", keys)
	}
	return cfg, nil
}

// findConfig resolves the config path (--config flag or default) and loads it.
// For the default path only: if no config exists, a template is written there
// and the function returns errConfigCreated. If the config is empty,
// errUninstallRequested is returned.
func findConfig() (string, appConfig, error) {
	if *configFileFlag != "" {
		cfg, err := loadConfig(*configFileFlag)
		if err != nil {
			return "", appConfig{}, fmt.Errorf("load config %s: %w", *configFileFlag, err)
		}
		return *configFileFlag, cfg, nil
	}
	if _, err := os.Stat(confPath); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(confPath, configTemplate, 0o600); err != nil {
			return "", appConfig{}, fmt.Errorf("write config template: %w", err)
		}
		return confPath, appConfig{}, errConfigCreated
	}
	cfg, err := loadConfig(confPath)
	if err != nil {
		return "", appConfig{}, fmt.Errorf("load config %s: %w", confPath, err)
	}
	return confPath, cfg, nil
}

var errConfigCreated = errors.New("config template created")

var installFiles = []string{
	"/etc/udev/rules.d/90-kobodeck.rules",
	"/usr/local/bin/kobodeck",
}

// doUninstall removes the given files and logs the result.
// Refuses to run if binaryPath is not under /usr/local to prevent accidents.
func doUninstall(binaryPath string, files []string) {
	log.Println("uninstall requested, clearing myself out")
	if !strings.HasPrefix(binaryPath, "/usr/local") {
		log.Fatal("unexpected command path, aborting uninstall:", binaryPath)
	}
	var lastErr error
	for _, file := range files {
		if err := os.Remove(file); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("failed to remove %s: %s", file, err)
			lastErr = err
		} else {
			log.Printf("deleted %s", file)
		}
	}
	if lastErr != nil {
		log.Fatal("uninstall partially failed")
	}
}

// nickelRescan triggers a Nickel library rescan by simulating a USB plug/unplug
// via /tmp/nickel-hardware-status. The user will see a Connect/Cancel dialog;
// pressing Connect rescans immediately, Cancel still picks up changes on reboot.
func nickelRescan(statusPath string) error {
	log.Println("triggering Nickel rescan")
	if err := appendNickelEvent(statusPath, "add"); err != nil {
		return err
	}
	time.Sleep(10 * time.Second)
	return appendNickelEvent(statusPath, "remove")
}

func appendNickelEvent(path, event string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("%s event: open %s: %w", event, path, err)
	}
	if _, err := f.WriteString("usb plug " + event + "\n"); err != nil {
		return errors.Join(fmt.Errorf("%s event: write %s: %w", event, path, err), f.Close())
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("%s event: close %s: %w", event, path, err)
	}
	return nil
}

// acquireLock acquires an exclusive non-blocking flock on path.
// Returns an error if another instance is already running.
func acquireLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, errors.Join(errors.New("already running"), f.Close())
	}
	return f, nil
}

// runCheck prints the active configuration and lists bookmarks that would be
// synced, without downloading anything. Used by the --check flag.
func (a app) runCheck(w io.Writer) error {
	if _, err := io.WriteString(w, formatCheckConfig(a.cfg)+"Connecting to Readeck... "); err != nil {
		return fmt.Errorf("write check output: %w", err)
	}
	entries, err := a.readeck.listBookmarks(a.cfg.Fetch)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(w, "OK\n\n"+formatCheckEntries(a.cfg, entries)); err != nil {
		return fmt.Errorf("write check output: %w", err)
	}
	return nil
}

func writeCheckOutput(w io.Writer, cfg appConfig, entries []readeckBookmark) error {
	_, err := io.WriteString(w, formatCheckConfig(cfg)+"Connecting to Readeck... OK\n\n"+formatCheckEntries(cfg, entries))
	return err
}

func formatCheckConfig(cfg appConfig) string {
	output := fmt.Sprintf(`Configuration:
  URL:     %s
  Output:  %s
  Workers: %d
  Limit:   %d
  Delete:  %v
`, cfg.Server.URL, cfg.Output.Path, cfg.Fetch.Workers, cfg.Fetch.Limit, cfg.Output.Delete)
	if cfg.Fetch.Labels != "" {
		output += fmt.Sprintf("  Labels:  %s\n\n", cfg.Fetch.Labels)
	} else {
		output += "  Labels:  (all)\n\n"
	}
	return output
}

func formatCheckEntries(cfg appConfig, entries []readeckBookmark) string {
	labelFilter := make(map[string]bool)
	if cfg.Fetch.Labels != "" {
		for _, l := range strings.Split(strings.ToLower(cfg.Fetch.Labels), ",") {
			labelFilter[strings.TrimSpace(l)] = true
		}
	}

	var matched, skipped int
	var output string
	for _, entry := range entries {
		if len(labelFilter) > 0 && !matchesLabelFilter(labelFilter, entry.Labels) {
			skipped++
			continue
		}
		matched++
		output += fmt.Sprintf("  %s — %s\n", entry.ID, entry.Title)
	}
	output += fmt.Sprintf("\n%d bookmarks to sync", matched)
	if skipped > 0 {
		output += fmt.Sprintf(", %d skipped (label filter)", skipped)
	}
	return output + "\n"
}

type bookmarkStore interface {
	getBookmark(id string) (readeckBookmark, error)
	patchBookmark(id string, fields map[string]any) error
}

type nickelLibrary interface {
	readStatus(id, outputDir string) (bookStatus, error)
	isInCollection(id, outputDir, collection string) (bool, error)
}

type localBook struct {
	id   string
	path string
}

func listLocalBooks(outputDir string) ([]localBook, error) {
	files, err := filepath.Glob(strings.TrimSuffix(outputDir, "/") + "/*.kepub.epub")
	if err != nil {
		return nil, err
	}
	books := make([]localBook, 0, len(files))
	for _, file := range files {
		books = append(books, localBook{
			id:   strings.TrimSuffix(filepath.Base(file), ".kepub.epub"),
			path: file,
		})
	}
	return books, nil
}

// reconcileLocalFiles checks each local EPUB against the Nickel DB and Readeck.
// Books marked as read in Nickel are marked as read in Readeck and optionally
// archived. When FavouriteCollection is configured, Readeck favourite state
// mirrors Kobo shelf membership, including for archived bookmarks. Books no
// longer in the fetched feed are deleted when allowDelete is set, unless
// currently being read.
func reconcileLocalFiles(
	readeck bookmarkStore,
	nickel nickelLibrary,
	cfg appConfig,
	valid map[string]bool,
	bookmarks map[string]readeckBookmark,
	allowDelete bool,
) (bool, error) {
	filesChanged := false
	outputDir := strings.TrimSuffix(cfg.Output.Path, "/")
	books, err := listLocalBooks(outputDir)
	if err != nil {
		return false, fmt.Errorf("cannot list local books: %w", err)
	}
	debugf(cfg.Log.Verbose, "local books to inspect: %v", books)
	var reconcileErr error
	for _, book := range books {
		changed, err := reconcileLocalBook(readeck, nickel, cfg, outputDir, book, valid, bookmarks, allowDelete)
		if err != nil {
			log.Printf("warning: failed to reconcile %s: %s", book.path, err)
			reconcileErr = errors.Join(reconcileErr, fmt.Errorf("%s: %w", book.path, err))
		}
		if changed {
			filesChanged = true
		}
	}
	return filesChanged, reconcileErr
}

func reconcileLocalBook(
	readeck bookmarkStore,
	nickel nickelLibrary,
	cfg appConfig,
	outputDir string,
	book localBook,
	valid map[string]bool,
	bookmarks map[string]readeckBookmark,
	allowDelete bool,
) (bool, error) {
	var reconcileErr error
	uid := book.id
	if uid == "" {
		log.Println("skipping file with empty name:", book.path)
		return false, nil
	}
	status, statusErr := nickel.readStatus(uid, outputDir)
	var inCollection bool
	collectionKnown := cfg.Sync.FavouriteCollection == ""
	if cfg.Sync.FavouriteCollection != "" {
		var err error
		inCollection, err = nickel.isInCollection(uid, outputDir, cfg.Sync.FavouriteCollection)
		if err != nil {
			log.Println("failed to check collection:", err)
			reconcileErr = errors.Join(reconcileErr, fmt.Errorf("check collection: %w", err))
		} else {
			collectionKnown = true
		}
	}
	if statusErr != nil {
		// Skip entirely — don't delete a book we can't confirm the read state of.
		log.Println(statusErr)
		return false, errors.Join(reconcileErr, fmt.Errorf("read status: %w", statusErr))
	}
	bookmark, bookmarkKnown := bookmarks[uid]
	if status == bookRead && !bookmarkKnown {
		var err error
		bookmark, err = readeck.getBookmark(uid)
		if err != nil {
			log.Printf("cannot read Readeck bookmark %s: %v", uid, err)
			reconcileErr = errors.Join(reconcileErr, fmt.Errorf("get bookmark: %w", err))
		} else {
			bookmarkKnown = true
		}
	}
	if status == bookRead && bookmarkKnown {
		fields := make(map[string]any)
		action := "read"
		if bookmark.ReadProgress != 100 {
			fields["read_progress"] = 100
		}
		if cfg.Sync.Archive {
			fields["is_archived"] = true
			action += " and archived"
		}
		if len(fields) > 0 {
			log.Printf("marking entry %s as %s", uid, action)
			if err := readeck.patchBookmark(uid, fields); err != nil {
				log.Printf("failed to mark entry %s as %s: %v", uid, action, err)
				reconcileErr = errors.Join(reconcileErr, fmt.Errorf("mark read: %w", err))
			} else if cfg.Sync.Archive {
				valid[uid] = false
			}
		}
	}
	if collectionKnown && cfg.Sync.FavouriteCollection != "" && !bookmarkKnown {
		var err error
		bookmark, err = readeck.getBookmark(uid)
		if err != nil {
			log.Printf("cannot read Readeck favourite state for %s: %v", uid, err)
			reconcileErr = errors.Join(reconcileErr, fmt.Errorf("get favourite state: %w", err))
		} else {
			bookmarkKnown = true
		}
	}
	if collectionKnown && bookmarkKnown && cfg.Sync.FavouriteCollection != "" &&
		inCollection != bookmark.IsMarked {
		action := "marking"
		if !inCollection {
			action = "unmarking"
		}
		log.Printf("%s entry %s as favourite", action, uid)
		if err := readeck.patchBookmark(uid, map[string]any{"is_marked": inCollection}); err != nil {
			log.Printf("failed to set favourite state to %t: %v", inCollection, err)
			reconcileErr = errors.Join(reconcileErr, fmt.Errorf("set favourite state: %w", err))
		}
	}
	if allowDelete && !valid[uid] {
		// Keep the local book as retry input whenever its remote state may not
		// have been reconciled successfully.
		if reconcileErr != nil {
			return false, reconcileErr
		}
		if status == bookReading || status == bookClosed {
			log.Printf("not deleting book currently being read: %s", book.path)
		} else if err := os.Remove(book.path); err != nil {
			return false, errors.Join(reconcileErr, fmt.Errorf("delete book: %w", err))
		} else {
			log.Println("deleted", book.path)
			return true, nil
		}
	}
	return false, reconcileErr
}
