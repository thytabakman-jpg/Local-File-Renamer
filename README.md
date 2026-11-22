# Local File Renamer 3.5

Local File Renamer for Windows x64 proposes filename changes and file moves, and checks for duplicates. Setup installs for the current user and adds a Start menu shortcut. The launcher opens the app at `http://127.0.0.1:39173/`.

## Scan and review

Choose a **Scan type** and a folder using the Windows picker or by pasting its path. The default scope is all file types, including extensionless files, with subfolders included. The setup page shows when a saved profile narrows the scope. Choose **All file types** to clear profile file, path and size restrictions for this run, or **Selected file types** to choose extensions. The scanner works within the selected folder and skips links and junctions.

**Start new scan** builds a new file list. **Start fresh** also rereads files without using the assessment cache. **Resume saved scan** continues with the saved file list and scope. The inventory reports how many files it found, included and filtered out, plus folder counts. Open **Review** to inspect scan results. If the model is unavailable, new scans and resumes can use built-in analysis.

- **File Renamer** proposes filename changes. Use **Run without local AI** for deterministic inspection or download a local model for image and assisted document analysis. You can also opt into folder renames and supported metadata repairs.
- **Sorter** compares a file's evidence with folders in the scan and groups proposals by destination. It can suggest new subfolders when several files share a topic. Open **Review proposed moves** when a scan completes, then review the folder tree and select which files to move. A scan without proposals explains the effective scope or why it found no eligible destination. Apply creates approved destination folders as needed and retains the original filenames. Undo returns moved files to their source folders.
- **Duplicate Scanner** groups byte-identical files by SHA-256 and recommends cleanup for supported files with verified equivalent contents. It runs without a model and reports results without changing files.

A run can be paused, resumed, or opened from **Run history**. **Close app** saves completed work and stops the model. Windows Setup installs each version into a new program directory and updates the Start menu shortcut. If an earlier Renamer is still running, close it before opening the new version. Models, profiles, assessment cache, and run history are stored separately from the program files.

In **Exact duplicates**, the app proposes a keeper per group, preferring filenames without a copy suffix and breaking ties by alphabetical path. You can change the keeper and uncheck extras. **Accept recommendations** reviews the selected groups together and sends approved extras to the Windows Recycle Bin.

**Recommended cleanup** lists copies whose complete contents match apart from a verified difference. It covers Windows line endings in UTF-8 TXT and Markdown files up to 1 MiB and fixed-width creation or modification timestamps in supported PDFs up to 16 MiB. A PDF match requires every other byte to match, including page contents, annotations and attachments. Similar filenames and matching excerpts can contribute to the count of candidates left alone. Each recommendation shows what matched, which file to keep and what to recycle. **Approve** or **Approve all** presents the exact paths before recycling. The app rechecks approved files and shows a receipt. Preview-only scans keep recycling disabled. To get these recommendations for files checked in 3.4, start a new scan. When exact and equivalent copies overlap, clear the exact duplicates first to reveal the remaining cleanup recommendation.

## Review a scan

Use the top navigation for New scan, Review, History, Settings, and Local chat. In File Renamer review, select a file to see a local image or document preview and the filename and metadata comparison. PDF previews render a page at a time. DOCX and DOCM previews show a simplified, read-only layout of extracted paragraphs and headings. Macros and external document links are never executed. A file preview can be unavailable when a decoder is missing or the file cannot be read.

Selecting a row opens its details. In Proposed and Flagged, check the box beside a ready change to include it in Apply selected, or apply it from the file details. Ready files can be applied during a scan. Unchecked proposals remain available. A failed write stays available for retry. Edits, apply, and undo use the run journal, with source and destination checks to prevent overwrites. Copy bug report in Review or History gives you a preview of the report and lets you include paths when needed.

In **Flagged**, select an assessed, unapplied file and click **Recommended names** to get up to four choices from its file evidence. **Apply chosen name** applies your selected recommendation. **Move to Recycle Bin** asks you to confirm moving the selected file. **Automatically changed** shows the original name, new name and naming rule, including changes later undone. Restore recycled files through Windows.

## Audio and video

Enable **Sample audio and video locally** for a scan with local AI. Put FFmpeg on your PATH to extract up to three video frames and a short audio clip. For speech, also put **whisper-cli** from whisper.cpp on your PATH, download a compatible Whisper GGML model, and set the Windows user environment variable **LOCAL_RENAMER_WHISPER_MODEL** to that model's full path. Reopen the Renamer after configuring these tools. The installer includes the existing vision engine. FFmpeg and Whisper are separately installed.

The app samples frames near 0, 12, and 30 seconds and up to the first 25 seconds of audio. It analyzes those samples locally. A recording with an unrepresentative opening may need a human title. Missing tools or ambiguous samples appear in the assessment. Video preview shows a still frame. For an image with a species label, the local model can compare the name against visible text before proposing it. Image versions require evidence of a relationship. An unambiguous same-folder original and background-removal export get reviewable Version 1 and Version 2 suggestions.

## Local chat

Open **Local chat** to control the current scan and inspect saved runs. Commands such as **scan scope**, **scan diagnostics**, **scan status**, **show scan history**, and **show exact duplicates** run without loading a language model. Use **start fresh** to reread the selected folder or say **“Find out why it only found nine, then start a fresh preview of every file in this folder and its subfolders.”**

With a local model installed, describe the folder, scan type, filters, and results you want in plain language. Chat can choose a recent folder, compare saved runs, explain and revise proposals, and reassess flagged files. **“Make that my default”** saves the selected run's scope to the active profile. Apply and undo use a review button for the exact operation. The default chat model is Qwen3.5-4B, and Qwen2.5-VL-3B is also selectable. The app uses one model process at a time.

For an image-only run, say **“Scan every image in this folder and name each by the subject you can see.”** The app uses the selected vision model and your naming guidance.


## Online lookups

Online lookup is enabled by default and can be changed under **Scan settings → Evidence depth**. Try **Search online for history of Toronto**, **Search online for DOI 10.1073/pnas.0908374106**, or **Verify publication date for proposal 0**. Show proposals to find the number for your file, or use **Verify date** on the file’s inspection row. These commands use the app's lookup tools directly, even while the local model is busy.

The app uses Crossref for DOI and title-author matches and Open Library for ISBN editions. A confirmed year can appear in an unresolved filename proposal and the scan report with its source. When the title cannot be matched or publication years conflict, the app reports the uncertainty. Online date suggestions require review before renaming.

General queries show Wikipedia results and a browser link for wider search. Set the Windows user variable `RENAMER_BRAVE_SEARCH_KEY` and restart the Renamer to get general web results within chat through the Brave Search API. Lookups send only keywords or identifiers such as a DOI, ISBN, or publication title and author. File processing stays local.


## Naming and automatic changes

| Type | Default filename pattern |
| --- | --- |
| Journal article | Author(s) - Year - Article Title - Journal |
| Verified arXiv preprint | Author(s) - Version Year - Article Title - arXiv ID |
| Academic book | Author(s) - Year - Book Title - Publisher |
| Other book | Author(s) - Year - Book Title |
| Course paper | Paper Title - Course Code - Version |

The classification checklist tests journal article, academic book, other book, and course paper in order. It records the evidence for a match or for continuing to the next check. With no confirmed match, the Renamer can propose a general descriptive name. Publication detection uses citation signals in the file. Course rules require positive course evidence, such as a course-code field on a cover sheet or a recognized code in a structured coursework filename. Authorship alone does not establish coursework.

A meaningful publication filename is retained when the proposed change would remove citation or descriptive details, including when fields cannot be verified. Missing citation fields appear in the report. Placeholder names may be previewed for generic source names. Automatic application requires supported fields, a lossless change, and an enabled category. Verified preprints use the article category. Automatic scans wait for PDF evidence before proposing names, including for files with code-like basenames. The correction controls can record a reviewed classification or replacement filename. Reassessment can reuse cached evidence.

Readers cover text-based PDFs, modern Office/OpenDocument files, EPUB, ZIP/TAR listings, UTF-8 text, JPEG including JFIF and JPE, PNG, GIF, BMP, TIFF, WebP, and MP3/MP4-family title tags. Windows OCR can sample rendered PDF pages with the local model. The supported metadata edits are verified title and author fields in eligible PDFs and DOCX/DOCM files. The original bytes are retained for undo.

## Settings, profiles, and comparison

**Scan settings and profiles** controls scope, naming, evidence depth, AI, resources, recovery, cache, and execution permissions. Save a named profile and activate it when needed. A paused scan resumes with its saved scope and settings. Reassessment uses current assessment settings while respecting the saved run's scope and permissions. Evidence workers prepare files concurrently when configured. Inference remains serial, and the image decoding budget is per worker. The program does not impose a hard RAM limit on llama.cpp.

The app stores bounded excerpts and resized images in its local assessment cache when caching is enabled. Rules, evidence settings, file changes, and model versions invalidate the affected reuse. The inspection and timing reports omit source excerpts and image bytes. The completion receipt lists completed, pending, skipped and failed work and counts unassessed files separately.

For a controlled comparison, enable **Record file hashes for a controlled comparison** in both scan profiles, copy the same starting files into two separate folders, and run a scan on each. **Compare scans** verifies starting relative paths and SHA-256 hashes before showing naming, classification, warning, timing and AI contribution differences.

## Optional assistant connection

**Allow ChatGPT to inspect and control this scan** is on when the app opens. Uncheck it to disable the connection for that session. Configure a local STDIO MCP client to launch the installed `LocalFileRenamer.exe` with `--mcp`. Use the new executable path from the Start menu shortcut properties. New installations are stored under a versioned folder in `%LOCALAPPDATA%\Programs`. The connection is authenticated and uses loopback. Check **renamer_capabilities** for the live app version and API. Status and aggregate reports omit filenames. Proposals and optional detailed reports contain paths.

The connection can start and pause scans, read proposed changes and reports, manage profiles and rules, correct classifications or filenames, reassess selections, apply authorised selections, and undo an identified run. For example, a single `renamer_start` call with a folder, `scope_mode: "selected"`, `include_extensions: [".pdf"]`, and `changes: {"execution.mode": "review"}` starts a PDF-only run without altering the active profile. Use the returned job ID with `renamer_status`. Use `renamer_get_settings` with specific field names for compact reads and `renamer_patch_settings` to save or reset an existing profile. The app and connection share validation, conflict checks, and the undo journal. The app can scan and apply changes independently.

## Build

The MIT-licensed source includes `build-windows.sh`. It requires Go 1.27.4, Python 3, a POSIX shell, and the pinned [llama.cpp b12588](https://github.com/ggml-org/llama.cpp/releases/tag/b12588) CPU archive. Set `LLAMA_CPU_ZIP` to reuse a verified copy of the archive. Setup can offer Microsoft's signed Visual C++ runtime if it is missing.

The application source and embedded browser interface are in `app/`. The Windows setup program is in `installer/`, and third-party licence terms are in `licenses/`. Run `go test ./...` for the Linux tests or `go run ./app` to start the app on a supported platform. Run `./build-windows.sh` to create `build/LocalFileRenamer-Installer-3.5.zip`. The ZIP contains a single Setup executable. To try real inference, set `LOCAL_RENAMER_ENGINE` to the llama.cpp directory and `LOCAL_RENAMER_MODELS` to the model files directory. Go modules, the engine archive, and GGUF weights are obtained separately from their pinned sources.
