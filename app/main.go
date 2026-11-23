package main

import (
	"context"
	"embed"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

//go:embed ui/*
var assets embed.FS

func main() {
	if len(os.Args) == 3 && os.Args[1] == "--choose-folder" {
		path, err := nativeFolderPicker(os.Args[2])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Fprint(os.Stdout, path)
		return
	}
	home, err := dataHome()
	if err != nil {
		showError(err)
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "--mcp" {
		runMCP(home, os.Stdin, os.Stdout)
		return
	}
	exe, err := os.Executable()
	if err != nil {
		showError(err)
		return
	}
	backend := filepath.Join(filepath.Dir(exe), "engine")
	if override := os.Getenv("LOCAL_RENAMER_ENGINE"); override != "" {
		backend = override
	}
	models := filepath.Join(home, "models")
	if override := os.Getenv("LOCAL_RENAMER_MODELS"); override != "" {
		models = override
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:39173")
	if err != nil {
		res, probeErr := (&http.Client{Timeout: 2 * time.Second, Transport: loopbackTransport}).Get("http://127.0.0.1:39173/status")
		if probeErr == nil {
			b, _ := io.ReadAll(io.LimitReader(res.Body, 100))
			res.Body.Close()
			running := strings.TrimSpace(string(b))
			if running == "Local File Renamer v"+appVersion {
				_ = openURL("http://127.0.0.1:39173/")
				return
			}
			if strings.HasPrefix(running, "Local File Renamer v") {
				showError(fmt.Errorf("%s is still running. Use Close app in its browser window at http://127.0.0.1:39173/, then reopen Local File Renamer from the Start menu", running))
				return
			}
		}
		showError(fmt.Errorf("The app's local port is in use: %w", err))
		return
	}
	defer listener.Close()
	a, err := newApp(home, backend, models)
	if err != nil {
		showError(err)
		return
	}
	defer a.chat.cancel()
	srv := server(a)
	if os.Getenv("LOCAL_RENAMER_HEADLESS") == "" {
		go func() {
			time.Sleep(130 * time.Millisecond)
			if err := openURL("http://127.0.0.1:39173/"); err != nil {
				showError(err)
			}
		}()
	}
	if err = srv.Serve(listener); err != nil && err != http.ErrServerClosed {
		showError(err)
	}
}

func server(a *application) *http.Server {
	nonce := token()
	aFS, _ := fs.Sub(assets, "ui")
	mux := http.NewServeMux()
	static := http.FileServer(http.FS(aFS))
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "Local File Renamer v"+appVersion) })
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.SetCookie(w, &http.Cookie{Name: "local_renamer", Value: nonce, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
			w.Header().Set("Cache-Control", "no-store")
		}
		switch r.URL.Path {
		case "/app.js", "/chat.js", "/operations.js", "/review.js", "/console.js":
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		case "/style.css":
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
		}
		w.Header().Set("Cache-Control", "no-store")
		static.ServeHTTP(w, r)
	})
	auth := func(w http.ResponseWriter, r *http.Request) bool {
		cookie, err := r.Cookie("local_renamer")
		if err != nil || cookie.Value != nonce {
			http.Error(w, "Open the app window first", http.StatusForbidden)
			return false
		}
		if origin := r.Header.Get("Origin"); r.Method == "POST" && origin != "" && origin != "http://127.0.0.1:39173" {
			http.Error(w, "Request must come from the app", http.StatusForbidden)
			return false
		}
		return true
	}
	mux.HandleFunc("GET /api/apply-all", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		q := r.URL.Query()
		revision, err := strconv.ParseUint(q.Get("revision"), 10, 64)
		if err != nil {
			http.Error(w, "Provide the current proposal revision", http.StatusBadRequest)
			return
		}
		a.mu.Lock()
		indices, err := a.applyAllIndices(q.Get("job"), revision, q.Get("filter"), q.Get("folder"))
		a.mu.Unlock()
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		reply(w, map[string]int{"count": len(indices)})
	})
	a.registerTestRoutes(mux, auth)
	mux.HandleFunc("POST /api/chat", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		var req struct {
			chatRequest
			Approve string
			Reset   bool
		}
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") || json.NewDecoder(http.MaxBytesReader(w, r.Body, 40<<10)).Decode(&req) != nil {
			http.Error(w, "Invalid local chat request", 400)
			return
		}
		var response chatReply
		var err error
		switch {
		case req.Reset:
			err = a.resetChat()
		case req.Approve != "":
			err = a.approveChat(req.Approve)
			response.Message = "The approved operation started. See scan status for the result."
		default:
			response, err = a.talk(r.Context(), req.chatRequest)
		}
		if err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
		reply(w, response)
	})
	mux.HandleFunc("GET /api/chat-scan", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		q := r.URL.Query()
		response, err := a.chatScanUpdate(q.Get("id"), q.Get("exact") == "1", q.Get("results") == "1")
		if err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
		reply(w, response)
	})
	mux.HandleFunc("GET /api/file-preview", func(w http.ResponseWriter, r *http.Request) {
		if auth(w, r) {
			a.serveFilePreview(w, r)
		}
	})
	mux.HandleFunc("GET /api/recommended-names", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		q := r.URL.Query()
		index, indexErr := strconv.Atoi(q.Get("index"))
		revision, revisionErr := strconv.ParseUint(q.Get("revision"), 10, 64)
		if indexErr != nil || revisionErr != nil {
			http.Error(w, "Select a file from the current review", 400)
			return
		}
		names, err := a.recommendedNames(r.Context(), q.Get("job"), revision, index)
		if err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
		reply(w, names)
	})
	mux.HandleFunc("POST /api/bridge", a.bridge)
	mux.HandleFunc("GET /api/diagnostics", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		value, err := a.diagnostic(r.URL.Query().Get("job"), true)
		if err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
		reply(w, value)
	})
	mux.HandleFunc("GET /api/kept-locations", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		q := r.URL.Query()
		offset, err := strconv.Atoi(q.Get("offset"))
		revision, revErr := strconv.ParseUint(q.Get("revision"), 10, 64)
		if err != nil || revErr != nil {
			http.Error(w, "Choose the current Move File results", 400)
			return
		}
		page, err := a.bridgeResults(bridgeCommand{Job: q.Get("job"), Filter: "kept_locations",
			Offset: offset, Revision: &revision, IncludeNames: true})
		if err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
		reply(w, page)
	})
	mux.HandleFunc("GET /api/model-evidence", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		q := r.URL.Query()
		index, err := strconv.Atoi(q.Get("index"))
		if err != nil {
			http.Error(w, "Choose an assessed file", 400)
			return
		}
		data, err := a.modelTrace(q.Get("job"), index)
		if err != nil {
			http.Error(w, "No model evidence was saved for this assessment", 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=model-evidence-%d.json", index))
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(data)
	})
	mux.HandleFunc("GET /api/settings", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		info, err := a.settingsInfo(r.URL.Query().Get("profile"))
		if err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
		reply(w, info)
	})
	mux.HandleFunc("GET /api/rules", func(w http.ResponseWriter, r *http.Request) {
		if auth(w, r) {
			reply(w, a.getRules())
		}
	})
	mux.HandleFunc("GET /api/compare", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		q := r.URL.Query()
		offset, _ := strconv.Atoi(q.Get("offset"))
		value, err := a.compareRuns(q.Get("first"), q.Get("second"), max(0, offset), true)
		if err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
		reply(w, value)
	})
	mux.HandleFunc("GET /api/report", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		report, err := a.report(r.URL.Query().Get("job"))
		if err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
		reply(w, report)
	})
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		if auth(w, r) {
			reply(w, a.snapshot())
		}
	})
	mux.HandleFunc("GET /api/preview", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		page, err := a.proposalPage(r.URL.Query().Get("job"), max(0, offset))
		if err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
		reply(w, page.Rows)
	})
	mux.HandleFunc("GET /api/inspection", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		page, err := a.reviewInspections(max(0, offset), r.URL.Query().Get("unknown") == "1", r.URL.Query().Get("filter"))
		if err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
		reply(w, page)
	})
	csvDownload := func(w http.ResponseWriter, filename string, export func(*csv.Writer) error) {
		var data strings.Builder
		if err := export(csv.NewWriter(&data)); err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
		fmt.Fprint(w, "\uFEFF"+data.String())
	}
	mux.HandleFunc("GET /api/flags", func(w http.ResponseWriter, r *http.Request) {
		if auth(w, r) {
			csvDownload(w, "renaming-exceptions.csv", a.export)
		}
	})
	mux.HandleFunc("GET /api/inspection.csv", func(w http.ResponseWriter, r *http.Request) {
		if auth(w, r) {
			unknown := r.URL.Query().Get("unknown") == "1"
			filename := "file-inspection.csv"
			if unknown {
				filename = "unknown-publication-fields.csv"
			}
			csvDownload(w, filename, func(c *csv.Writer) error { return a.exportInspections(c, unknown) })
		}
	})
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 45 * time.Second, MaxHeaderBytes: 16 << 10}
	mux.HandleFunc("GET /api/compare-duplicates", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		q := r.URL.Query()
		first, firstErr := strconv.Atoi(q.Get("first"))
		second, secondErr := strconv.Atoi(q.Get("second"))
		if firstErr != nil || secondErr != nil {
			http.Error(w, "Choose two files", http.StatusBadRequest)
			return
		}
		report, err := a.compareDuplicateFiles(r.Context(), q.Get("job"), first, second)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		reply(w, report)
	})
	mux.HandleFunc("GET /api/scan-results", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		results, err := a.scanResults(r.URL.Query().Get("job"))
		if err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
		reply(w, results)
	})
	mux.HandleFunc("POST /api/action", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			http.Error(w, "JSON action required", 415)
			return
		}
		body := http.MaxBytesReader(w, r.Body, 2<<20)
		var q struct {
			Action, Root, ID                     string
			Operation                            scanOperation
			Partial, Folders, Auto, Repair, Sort bool
			Start, End, Index                    int
			Enabled                              bool
			Excluded, Indices                    []int
			IncludeExtensions                    []string
			ScopeMode, RequestID                 string
			Subfolders                           *bool
			Fresh                                bool
			Revision                             *uint64
			Keep                                 *int
			DuplicateGroups, VerifiedGroups      []duplicateSelection
			ChosenNames                          []chosenName
			Count                                int
			RowRevisions                         map[int]uint64
			Rules                                *namingRules
			Filename                             *string
			Classification, Group, Value         string
			Refresh                              bool
			Profile                              string
			Settings                             *scanSettings
			Activate, PreviewOnly                bool
			MediaSamples                         *bool
		}
		if err := json.NewDecoder(body).Decode(&q); err != nil {
			http.Error(w, "Invalid action", 400)
			return
		}
		var err error
		switch q.Action {
		case "pick":
			var root string
			root, err = a.pick(r.Context(), q.Root)
			if err == nil {
				reply(w, map[string]string{"root": root})
				return
			}
		case "start":
			if q.Root == "" {
				err = fmt.Errorf("Pick a folder first")
			} else {
				err = a.startConfiguredScan(q.Root, q.Partial, q.Folders, q.Auto, q.Repair, q.Sort, q.IncludeExtensions, q.MediaSamples, q.PreviewOnly, q.Operation, scanStartOptions{
					ScopeMode: q.ScopeMode, Subfolders: q.Subfolders, Fresh: q.Fresh, RequestID: q.RequestID, FallbackAI: true,
				})
			}
		case "resume":
			err = a.resumeWithFallback(q.ID, q.Partial, true)
		case "apply":
			if q.Revision != nil {
				a.mu.Lock()
				if a.v.Revision != *q.Revision {
					err = fmt.Errorf("The preview changed. Review it again before applying")
				} else {
					err = a.applyLocked(q.ID, q.Start, q.End, q.Excluded)
				}
				a.mu.Unlock()
			} else {
				err = a.apply(q.ID, q.Start, q.End, q.Excluded)
			}
		case "apply-selected":
			if q.Revision == nil {
				err = fmt.Errorf("Provide the current proposal revision")
			} else {
				a.mu.Lock()
				err = a.applySelectionLocked(q.ID, *q.Revision, q.Indices, q.RowRevisions)
				a.mu.Unlock()
			}
		case "apply-all":
			if q.Revision == nil {
				err = fmt.Errorf("Provide the current proposal revision")
			} else {
				err = a.applyAllReviewed(q.ID, *q.Revision, q.Group, q.Root, q.Count)
			}
		case "apply-chosen-names":
			err = a.applyChosenNames(q.ID, q.ChosenNames)
		case "change-location":
			if q.Revision == nil {
				err = fmt.Errorf("Provide the current proposal revision")
			} else {
				err = a.changeLocation(q.ID, *q.Revision, q.Index, q.Root)
			}
		case "correct":
			if q.Revision == nil {
				err = fmt.Errorf("Provide the current proposal revision")
			} else {
				err = a.correct(q.ID, *q.Revision, q.Index, q.Classification, q.Filename)
			}
		case "reassess":
			if q.Revision == nil {
				err = fmt.Errorf("Provide the current proposal revision")
			} else {
				err = a.reassess(q.ID, *q.Revision, q.Indices, q.Group, q.Value, q.Refresh)
			}
		case "save-profile", "restore-profile":
			if q.Revision == nil || q.Action == "save-profile" && q.Settings == nil {
				err = fmt.Errorf("Provide the current profile revision and complete settings")
			} else {
				err = a.saveProfile(*q.Revision, q.Profile, q.Settings, q.Activate || q.Action == "restore-profile")
			}
		case "rules":
			if q.Revision == nil || q.Rules == nil {
				err = fmt.Errorf("Provide the current rule revision and complete naming rules")
			} else {
				err = a.updateRules(*q.Revision, *q.Rules)
			}
		case "open-pdf":
			path, _, readErr := a.previewFile(q.ID, q.Index)
			if readErr != nil {
				err = readErr
			} else if !strings.EqualFold(extension(path), ".pdf") {
				err = fmt.Errorf("Choose a PDF to open")
			} else {
				err = openURL(path)
			}
		case "use-review-name":
			if q.Revision == nil || q.Filename == nil {
				err = fmt.Errorf("Provide a filename and the current proposal revision")
			} else {
				err = a.useReviewName(q.ID, *q.Revision, q.Index, *q.Filename)
			}
		case "apply-one":
			err = a.applyOneRevision(q.ID, q.Index, q.Revision)
		case "recycle-duplicates", "recycle-duplicate-groups", "recycle-verified-duplicates", "recycle-all-recommendations":
			if !recycleAvailable {
				err = fmt.Errorf("The Windows Recycle Bin is unavailable")
				break
			}
			if q.Revision == nil {
				err = fmt.Errorf("Confirm copies from the current duplicate scan")
				break
			}
			if q.Action == "recycle-duplicates" {
				if q.Keep == nil {
					err = fmt.Errorf("Choose a file to keep")
					break
				}
				count, recycleErr := a.recycleMatchedCopies(r.Context(), q.ID, *q.Revision, *q.Keep, q.Indices, recycleFile)
				if recycleErr != nil {
					err = fmt.Errorf("%d recycled. %w", count, recycleErr)
					break
				}
				reply(w, map[string]int{"recycled": count})
				return
			}
			var receipt duplicateRecycleReceipt
			var recycleErr error
			if q.Action == "recycle-all-recommendations" {
				receipt, recycleErr = a.recycleAllRecommendations(r.Context(), q.ID, *q.Revision, q.DuplicateGroups, q.VerifiedGroups, recycleFile)
			} else {
				receipt, recycleErr = a.recycleApprovedCopies(r.Context(), q.ID, *q.Revision, q.DuplicateGroups, q.Action == "recycle-verified-duplicates", recycleFile)
			}
			message := ""
			if recycleErr != nil {
				message = recycleErr.Error()
			}
			reply(w, struct {
				duplicateRecycleReceipt
				Error string
			}{receipt, message})
			return
		case "recycle-file":
			if q.Revision == nil || len(q.Indices) != 1 {
				err = fmt.Errorf("Confirm one file from the current review")
			} else {
				err = a.recycleFlaggedFile(q.ID, *q.Revision, q.Indices[0])
			}
		case "bridge":
			err = a.setBridge(q.Enabled)
		case "pause":
			a.pauseRun()
		case "undo":
			err = a.undo()
		case "model":
			err = a.download()
		case "select":
			err = a.selectJob(q.ID)
		case "open-data":
			err = openURL(a.home)
		case "exit":
			work, picker := a.requestClose()
			a.testMu.Lock()
			testDone := a.testDone
			a.testMu.Unlock()
			const accepted = "{\"ok\":true}\n"
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("Content-Length", strconv.Itoa(len(accepted)))
			_, _ = io.WriteString(w, accepted)
			_ = http.NewResponseController(w).Flush()
			go func() {
				for _, done := range []<-chan struct{}{work, picker, testDone} {
					if done != nil {
						<-done
					}
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = srv.Shutdown(ctx)
			}()
			return
		default:
			err = fmt.Errorf("Unknown action")
		}
		if err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
		reply(w, map[string]bool{"ok": true})
	})
	srv.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "127.0.0.1:39173" {
			http.Error(w, "Local requests only", 403)
			return
		}
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' blob:; object-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		mux.ServeHTTP(w, r)
	})
	return srv
}
func reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func openURL(path string) error {
	var c *exec.Cmd
	if runtime.GOOS == "windows" {
		if strings.HasPrefix(path, "http://127.0.0.1:39173/") {
			c = exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", path)
		} else {
			c = exec.Command("explorer.exe", path)
		}
	} else {
		c = exec.Command("xdg-open", path)
	}
	configureChild(c)
	return c.Run()
}
func pickFolder(ctx context.Context, folder string) (string, error) {
	var c *exec.Cmd
	if runtime.GOOS == "windows" {
		exe, err := os.Executable()
		if err != nil {
			return "", err
		}
		c = exec.CommandContext(ctx, exe, "--choose-folder", strings.TrimSpace(folder))
	} else {
		if _, err := exec.LookPath("zenity"); err == nil {
			c = exec.CommandContext(ctx, "zenity", "--file-selection", "--directory", "--title=Choose a folder")
		} else {
			return "", fmt.Errorf("Enter the folder path in the app")
		}
	}
	output, err := c.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if len(output) == 0 && runtime.GOOS != "windows" {
			return "", nil
		}
		return "", fmt.Errorf("Folder picker failed: %v %s", err, chars(string(output), 250))
	}
	return strings.TrimSpace(string(output)), nil
}
func showError(err error) {
	fmt.Fprintln(os.Stderr, err)
	if runtime.GOOS == "windows" {
		c := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command",
			`Add-Type -AssemblyName System.Windows.Forms; [void][System.Windows.Forms.MessageBox]::Show($env:RENAMER_START_ERROR, 'Local File Renamer')`)
		c.Env = append(os.Environ(), "RENAMER_START_ERROR="+err.Error())
		configureChild(c)
		_ = c.Run()
	}
}
