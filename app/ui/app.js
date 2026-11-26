"use strict";

// Section: Shared application state and configuration.

const el = (id) => document.getElementById(id);
const operationNames = {rename: "File Renamer", move: "Sorter", duplicates: "Duplicate Scanner"};
const phases = {
  ready: "Ready", indexing: "Finding files", starting: "Starting scan",
  files: "Reading files", folders: "Checking folders", paused: "Scan paused",
  preview: "Preview ready", applying: "Applying changes", metadata: "Repairing metadata", "apply-paused": "Apply paused", "legacy-paused": "Previous scan paused",
  complete: "Complete", reassessing: "Reassessing files", download: "Downloading model", undo: "Undoing changes",
  "undo-paused": "Undo paused", undone: "Changes undone"
};
let latest = {};
let lastJob = "";
let rowsKey = "";
let historyKey = "";
let closed = false;
let closing = false;
let folderEdited = false;
let actionMessage = "";
let pendingAction = false;
let pageOffset = 0;
let refreshSerial = 0;
let renderedSerial = 0;
const selectedItems = new Map();
let inspectionOffset = 0;
let inspectionKey = "";
let inspectionSerial = 0;
let profiles;
let scanProfile;
let pendingScan;
let scanStartRejected = false;
let profileLoading = false;
let correction;
let compareOffset=0;
const profileSections = {
  scope:"Scan scope", naming:"Naming preferences", evidence:"Evidence depth",
  ai:"Local AI", resources:"Computer resources", recovery:"Uncertainty and recovery",
  cache:"Cache and reassessment", execution:"Execution permissions"
};
const settingsLists = new Set(["include_extensions","exclude_extensions","include_paths","exclude_paths","selected_paths","protected","auto_kinds"]);

// Section: Notifications and inspection rendering.

function notice(message) {
  el("message").textContent = message || "";
  show("message", Boolean(message));
}

async function loadInspection(key, offset, unknown) {
  const serial = ++inspectionSerial;
  try {
    const query = `offset=${offset}&unknown=${unknown ? 1 : 0}`;
    const response = await fetch(`/api/inspection?${query}`, {cache:"no-store", signal:AbortSignal.timeout(5000)});
    loadReceipt(key);
    if (!response.ok) throw new Error((await response.text()).trim());
    const page = await response.json();
    if (closed || inspectionKey !== key || serial !== inspectionSerial) return;
    el("inspection-summary").textContent = page.Total
      ? `${offset + 1}–${offset + page.Rows.length} of ${page.Total.toLocaleString()} files`
      : (unknown ? "No files with unknown publication fields." : "No files recorded for this scan.");
    el("inspection-export").href = `/api/inspection.csv?unknown=${unknown ? 1 : 0}`;
    show("inspection-scroll", page.Total > 0);
    show("inspection-pages", page.Total > 100);
    el("inspection-prev").disabled = offset === 0;
    el("inspection-next").disabled = offset + 100 >= page.Total;
    el("inspection-rows").replaceChildren(...page.Rows.map(row => {
      const tr = document.createElement("tr");
      const checked = row.Inspection;
      const sources = [];
      if (checked.Text) sources.push("Text or file listing");
      if (checked.Metadata) sources.push("Embedded metadata");
      if (checked.Image) sources.push("Image decoded");
      sources.push("Local AI: " + (checked.AI || "Not recorded") + (checked.Model ? " (" + checked.Model + ")" : ""));
      if (checked.OCR) sources.push("OCR: " + checked.OCR);
      if (checked.Cache) sources.push(checked.Cache);
      if (checked.Detail && row.Note !== checked.Detail && /\.(mp3|mp4|mov|m4v|m4a|wav|flac|mkv|webm|avi|ogg|opus|aac)$/i.test(row.Path)) sources.push(checked.Detail);
      if (checked.AIContribution) sources.push("AI contribution: " + checked.AIContribution);
      if (row.Kind) sources.push(row.Kind.replaceAll("_", " ")); 
      if (checked.PublicationDate) sources.push("Publication date: " + checked.PublicationDate);
      for (const check of checked.Checklist || []) sources.push(`${check.Type} · ${check.Result}. ${check.Reason}`);

      const timings = fileTiming(checked, row.RenameMS);
      const note = [row.Outcome, row.Reason, row.Proposed && row.Path !== row.Proposed ? "Proposed: " + row.Proposed : "", row.Note].filter(Boolean).join("\n");
      for (const value of [row.Path, sources.join("\n"), timings, note]) {
        const td = document.createElement("td");
        td.textContent = value;
        if (value === sources.join("\n") && checked.PublicationSource?.startsWith("https://")) {
          const link = document.createElement("a");
          link.href = checked.PublicationSource;
          link.target = "_blank";
          link.rel = "noopener noreferrer";
          link.textContent = "Publication source";
          td.append(document.createElement("br"),link);
        }
        tr.append(td);
      }
      const controls = document.createElement("td");
      if (checked.ModelTrace) {
        const link = document.createElement("a");
        link.href = "/api/model-evidence?" + new URLSearchParams({job: page.Job, index: row.Index});
        link.textContent = "Export model evidence";
        controls.append(link);
      }
      if (row.Outcome !== "Skipped" && /\.(pdf|docx|docm|epub|odt)$/i.test(row.Path)) {
        const button = document.createElement("button");
        button.type = "button";
        button.className = "quiet";
        button.textContent = "Verify date";
        button.onclick = () => {
          selectView("chat");
          if (localChatBusy) return;
          const message = "Verify publication date for file " + row.Index;
          addChatMessage("You", message);
          sendChat({Message:message});
        };
        controls.append(button);
      }
      if (!latest.Busy && row.Outcome !== "Skipped" && row.Index >= latest.ReviewStart) {
        const id = latest.Job, revision = latest.Revision;
        for (const [label, handler] of [
          ["Correct", () => openCorrection(row, id, revision)],
          ["Reassess", () => action("reassess", {id, revision, indices:[row.Index],refresh:el("fresh-assessment").checked})]
        ]) {
          const button = document.createElement("button");
          button.type = "button"; button.className = "quiet"; button.textContent = label;
          button.onclick = handler; controls.append(button);
        }
      }
      tr.append(controls);
      return tr;
    }));
  } catch (error) {
    if (!closed && inspectionKey === key && serial === inspectionSerial)
      el("inspection-summary").textContent = "Could not load file inspection. " + error.message;
  }
}

// Section: Completion reporting and history comparison.

async function loadReceipt(key) {
  try {
    const response = await fetch("/api/report?" + new URLSearchParams({job:latest.Job}), {cache:"no-store",signal:AbortSignal.timeout(5000)});
    if (!response.ok) return;
    const report = await response.json();
    if (inspectionKey !== key || closed) return;
    const receipt = report.Receipt;
    const lines = ["Assessments","Renames","Metadata","Undo"].map(label => {
      const counts=receipt[label];
      return `${label} · ${counts.Completed} completed, ${counts.Pending} pending, ${counts.Skipped} skipped, ${counts.Failed} failed`;
    });
    if (receipt.NotAssessed) lines.push(`${receipt.NotAssessed} files were not assessed.`);
    if (receipt.UndoAvailable) lines.push(`${receipt.UndoAvailable} changes can be undone.`);
    const contributions = Object.entries(report.AIContribution).map(([name,n])=>`${name} (${n})`);
    if (contributions.length) lines.push("Local AI · " + contributions.join(", "));
    if (receipt.LastError) lines.push(receipt.LastError);
    el("completion-receipt").textContent = lines.join("\n");
  } catch (_) {}
}

async function compareScans(offset) {
  const first=el("compare-first").value, second=el("compare-second").value;
  try {
    const query=new URLSearchParams({first,second,offset});
    const response=await fetch("/api/compare?" + query,{cache:"no-store",signal:AbortSignal.timeout(10000)});
    if (!response.ok) throw new Error((await response.text()).trim());
    const result=await response.json();
    if (first!==el("compare-first").value || second!==el("compare-second").value) return;
    const ai = Object.entries(result.SecondAI).map(([label,count])=>`${label} (${count})`);
    el("comparison-result").textContent=`${result.Files} identical starting files. ${result.Changed} assessments differed. Active time ${duration(result.FirstMS)} and ${duration(result.SecondMS)}.` +
      (ai.length ? " Second run AI contribution: " + ai.join(", ") + "." : "");
    el("comparison-rows").replaceChildren(...result.Rows.map(row=>{
      const tr=document.createElement("tr");
      for (const fields of [[row.Path],[row.FirstName,row.FirstClass,row.FirstDecision,row.FirstWarning],[row.SecondName,row.SecondClass,row.SecondDecision,row.SecondWarning,row.AIContribution]]) {
        const td=document.createElement("td");
        td.textContent=fields.filter(Boolean).join("\n");
        tr.append(td);
      }
      return tr;
    }));
    compareOffset=result.NextOffset;
    show("comparison-scroll",result.Rows.length>0);
    show("compare-more",compareOffset>0);
  } catch (err) {
    el("comparison-result").textContent=err.message;
    show("comparison-scroll",false);
    show("compare-more",false);
  }
}
el("compare").onclick=()=>compareScans(0);
el("compare-more").onclick=()=>compareScans(compareOffset);

// Section: Correction and reassessment controls.

function openCorrection(row, id, revision, applyName = false) {
  correction = {id, revision, index:row.Index, applyName};
  show("correct-kind-label", !applyName);
  show("correct-kind", !applyName);
  el("save-correction").textContent = applyName ? "Apply name" : "Save correction";
  el("correct-path").textContent = row.Path;
  el("correct-kind").value = "";
  el("correct-name").value = row.Name || "";
  el("correct-dialog").showModal();
}

el("save-correction").onclick = async () => {
  const filename = el("correct-name").value.trim();
  const classification = el("correct-kind").value;
  if (!filename && !classification) return;
  const nameAction = correction.applyName && filename && !classification ? "use-review-name" : "correct";
  const {applyName, ...selection} = correction;
  if (await action(nameAction, {...selection, classification, ...(filename ? {filename} : {})})) el("correct-dialog").close();
};

el("reassess-group").onclick = () => {
  const group = window.prompt("Group by classification, extension, or decision", "extension");
  if (!["classification", "extension", "decision"].includes(group)) return;
  const value = window.prompt("Value to match, such as .pdf, article, or Insufficient evidence");
  if (value?.trim()) action("reassess", {id:latest.Job, revision:latest.Revision, group, value:value.trim(), refresh:el("fresh-assessment").checked});
};

// Section: Profile settings rendering and persistence.

function populateSettings(data, profile) {
  const nodes = [];
  for (const [group, label] of Object.entries(profileSections)) {
    const section = document.createElement("details");
    const summary = document.createElement("summary");
    summary.textContent = label;
    section.append(summary);
    section.dataset.settingGroup = group;
    for (const key of Object.keys(data.defaults[group])) {
      const field = group + "." + key;
      const spec = data.fields[field];
      const value = data.effective[group][key];
      const row = document.createElement("label");
      row.className = "setting-field";
      const caption = document.createElement("span");
      caption.textContent = spec.label;
      let input;
      if (spec.values) {
        input = document.createElement("select");
        for (const option of spec.values) input.add(new Option(option.trim() ? option : "(spaces)", option));
        input.value = value;
      } else if (settingsLists.has(key) || key === "instructions" || key === "spelling") {
        input = document.createElement("textarea");
        input.rows = 2;
        input.value = settingsLists.has(key) ? (value || []).join("\n") : key === "spelling" ? JSON.stringify(value || {}) : value;
        if (settingsLists.has(key)) input.placeholder = "One entry per line";
      } else {
        input = document.createElement("input");
        input.type = typeof value === "number" ? "number" : typeof value === "boolean" ? "checkbox" : "text";
        if (input.type === "checkbox") input.checked = value;
        else input.value = value;
        if (spec.min !== undefined) {input.min = spec.min; input.max = spec.max;}
      }
      input.dataset.setting = field;
      row.append(caption,input);
      section.append(row);
    }
    nodes.push(section);
  }
  el("profile-fields").replaceChildren(...nodes);
  if (typeof displaySettingsCategory === "function") displaySettingsCategory();
  el("profiles").replaceChildren(...data.profiles.map(name => new Option(name,name)));
  el("profiles").value = profile || data.active;
  el("profile-name").value = profile || data.active;
  const ai = data.effective.ai.model === "off" || data.effective.ai.invoke === "never" ? "Local AI is off." :
    `Configured model: ${data.configured_model}${data.model_ready ? " (ready)" : " (download needed)"}.`;
  el("profile-effective").textContent = `Active profile: ${data.active}. ${ai} ${data.when_effective}`;
}

async function loadSettings(profile = "", setScan = false) {
  if (profileLoading) return;
  profileLoading = true;
  try {
    const response = await fetch("/api/settings?" + new URLSearchParams({profile}), {cache:"no-store", signal:AbortSignal.timeout(5000)});
    if (!response.ok) throw new Error((await response.text()).trim());
    profiles = await response.json();
    populateSettings(profiles, profile);
    if (setScan) {
      scanProfile = profiles;
      resetScanOptions();
      if (latest.Phase) render(latest);
    }
  } catch (err) {
    notice(err.message);
  } finally { profileLoading = false; }
}

el("profiles").onchange = () => loadSettings(el("profiles").value);
el("restore-profile").onclick = async () => {
  if (profiles && await action("restore-profile", {profile:el("profiles").value, revision:profiles.revision})) await loadSettings("",true);
};
el("save-profile").onclick = async () => {
  if (!profiles) return;
  const settings = structuredClone(profiles.effective);
  try {
    for (const field of el("profile-fields").querySelectorAll("[data-setting]")) {
      const [group,key] = field.dataset.setting.split(".");
      if (!field.checkValidity()) {field.reportValidity();return;}
      settings[group][key] = settingsLists.has(key) ? field.value.split("\n").map(x=>x.trim()).filter(Boolean) :
        key === "spelling" ? JSON.parse(field.value || "{}") : field.type === "checkbox" ? field.checked :
        field.type === "number" ? Number(field.value) : field.value;
    }
    if (await action("save-profile", {profile:el("profile-name").value.trim(), revision:profiles.revision, settings, activate:true})) await loadSettings("",true);
  } catch (err) {notice(err.message);}
};


// Section: Timing and diagnostics.

function duration(ms) {
  if (ms < 1000) return ms + " ms";
  if (ms < 60000) return (ms / 1000).toFixed(1) + " s";
  const seconds = Math.floor(ms / 1000);
  return (seconds >= 3600 ? Math.floor(seconds / 3600) + " h " : "") +
    Math.floor(seconds % 3600 / 60) + " min " + seconds % 60 + " s";
}

function fileTiming(checked, rename) {
  return [[checked.ReadMS, "Reading"], [checked.OCRMS, "OCR"], [checked.StartupMS, "Model startup"], [checked.ModelMS, "Model request total"], [checked.PromptMS, "Prompt processing"], [checked.GenerationMS, "Generation"], [checked.OnlineMS, "Online lookup"], [rename, "Rename"]]
    .filter(([ms]) => ms > 0).map(([ms, label]) => label + " " + duration(ms)).join("\n") || "No timing recorded";
}

async function loadDiagnostics(key, job) {
  try {
    const response = await fetch("/api/diagnostics?" + new URLSearchParams({job}), {cache:"no-store", signal:AbortSignal.timeout(5000)});
    if (!response.ok) throw new Error((await response.text()).trim());
    const d = await response.json();
    if (inspectionKey !== key || closed) return;
    const totals = fileTiming(d, d.RenameMS).replaceAll("\n", " · ");
    el("diagnostic-summary").textContent = d.LastError ? totals + " · " + d.LastError : totals;
    el("slowest-files").replaceChildren(...d.Slowest.map(row => {
      const li = document.createElement("li");
      li.textContent = duration(row.Inspection.TotalMS + row.RenameMS) + " · " + row.Path +
        (row.Status === "Interrupted" ? " (interrupted)" : "") +
        " · " + fileTiming(row.Inspection, row.RenameMS).replaceAll("\n", ", ");
      return li;
    }));
    show("slowest-files", d.Slowest.length > 0);
  } catch (err) {
    if (inspectionKey === key && !closed) el("diagnostic-summary").textContent = err.message;
  }
}

// Section: Shared review and action utilities.

function canReview(state) {
  return !state.Pending && ["preview", "paused", "files", "folders"].includes(state.Phase);
}

function show(id, visible) {
  el(id).hidden = !visible;
}

async function action(name, fields = {}) {
  if (closed || closing || (pendingAction && name !== "exit")) return null;
  pendingAction = true;
  actionMessage = "";
  if (name === "model") notice("Starting model download…");
  try {
    const r = await fetch("/api/action", {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ action: name, ...fields }),
      ...(["pick","recycle-duplicates","recycle-duplicate-groups","recycle-verified-duplicates","recycle-all-recommendations","apply-all","apply-chosen-names"].includes(name) ? {} : {signal:AbortSignal.timeout(15000)})
    });
    if (!r.ok) throw new Error((await r.text()).trim());
    if (name === "exit") {
      closing = true;
      render(latest);
    }
    return await r.json();
  } catch (error) {
    actionMessage = error.message;
    if (!closed) notice(actionMessage);
    return null;
  } finally {
    pendingAction = false;
    await refresh();
  }
}

// Section: Scan state and inventory rendering.

function folderKey(path) {
  path = path.trim();
  if (/^[A-Za-z]:|^[\\/]{2}/.test(path)) {
    return path.replaceAll("\\", "/").replace(/\/+$/, "").toLowerCase();
  }
  return path.replace(/\/+$/, "");
}

function describeInventory(state) {
  const inventory = state.Inventory;
  if (!inventory) return `${(state.FileTotal || 0).toLocaleString()} saved file entries`;
  const counts = [
    `${inventory.Found.toLocaleString()} files found`, `${inventory.Included.toLocaleString()} included`,
    `${inventory.Filtered.toLocaleString()} filtered out`, `${inventory.Folders.toLocaleString()} subfolders`
  ];
  if (inventory.OmittedFolders) counts.push(`${inventory.OmittedFolders.toLocaleString()} subfolders outside scope`);
  if (inventory.Unavailable) counts.push(`${inventory.Unavailable.toLocaleString()} unavailable entries`);
  if (!inventory.Included && inventory.FileTypes?.length) counts.push("Types present: " + inventory.FileTypes.join(", "));
  return counts.join(" · ");
}

function render(s) {
  if (closed) return;
  if (s.Closing || closing) {
    closing = true;
    el("exit").textContent = "Closing…";
    for (const control of document.querySelectorAll("button, input, select")) control.disabled = true;
    notice("Saving your progress and closing the app.");
    return;
  }
  const isNewPreview = latest.Job !== s.Job;
  latest = s;
  if (isNewPreview) {
    pageOffset = 0;
    inspectionOffset = 0;
    selectedItems.clear();
    if (typeof reviewCacheKey !== "undefined") reviewCacheKey = "";
  }
  const started = pendingScan && pendingScan === s.StartID && s.StartJob && s.Job === s.StartJob;
  const failedStart = pendingScan && pendingScan === s.StartID && !s.Busy && s.StartError;
  if (started || failedStart) pendingScan = "";
  if (failedStart) {
    actionMessage = s.StartError;
    scanStartRejected = true;
  }
  const startFailed = !s.Busy && (scanStartRejected || Boolean(s.StartError));
  const hadWork = Boolean(s.Job) && !s.StartingRoot && !startFailed;
  if (s.Job && lastJob !== s.Job) {
    if (!folderEdited) el("folder").value = s.Root;
    if (s.Operation) el("scan-operation").value = s.Operation;
    lastJob = s.Job;
  }
  const chosenOperation = el("scan-operation").value;
  const partial = el("partial").checked || !s.ModelReady || scanProfile?.effective.ai.invoke === "never";
  const differentFolder = hadWork && folderKey(el("folder").value) !== folderKey(s.Root);
  const scanning = ["indexing", "starting", "files", "folders", "reassessing", "undo", "applying", "metadata"].includes(s.Phase);

  renderScanSetupControls(s, startFailed, chosenOperation);
  renderScanLifecycleControls(s, hadWork, differentFolder, chosenOperation, partial, scanning);
  const live = s.Busy && ["files","folders","starting"].includes(s.Phase);
  if (typeof updateReview === "function") updateReview(s);
  renderScanProgress(s, partial, hadWork, startFailed, differentFolder, chosenOperation);
  renderFlaggedItems(s, hadWork);
  renderInspectionReports(s, hadWork, live);
  renderRunHistory(s);
  if (typeof renderConsole === "function") renderConsole(s);
}

function renderScanSetupControls(s, startFailed, chosenOperation) {
  el("phase").textContent = startFailed ? "Scan failed" : s.Pausing ? "Pausing…" : (phases[s.Phase] || s.Phase);
  notice(actionMessage || (startFailed && s.StartError) || s.Message || (s.Phase === "legacy-paused" ? "Start a new scan to review changes from this folder." : ""));
  show("model-panel", chosenOperation !== "duplicates" && !s.ModelReady);
  show("rename-options", chosenOperation === "rename");
  for (const id of ["partial", "media-samples"]) {
    el(id).closest("label").hidden = chosenOperation === "duplicates";
  }
  el("scan-operation").disabled = s.Busy;
  el("scope-mode").disabled = el("subfolders").disabled = s.Busy;
  el("selected-scan-folder").textContent = el("folder").value.trim();
  show("selected-scan-folder", Boolean(el("folder").value.trim()));
  if (s.ModelName && s.DownloadSize) el("model-name").textContent = "Download " + s.ModelName + " once (" + (s.DownloadSize / 1e9).toFixed(1) + " GB).";
  show("download-progress", s.Phase === "download");
  el("download-progress").max = s.DownloadSize || 1;
  show("download-status", s.Phase === "download");
  if (s.Downloaded) {
    el("download-progress").value = s.Downloaded;
    el("download-status").textContent = `${(s.Downloaded / 1000000).toFixed(1)} of ${Math.round(s.DownloadSize / 1000000)} MB downloaded`;
  } else {
    el("download-progress").removeAttribute("value");
    el("download-status").textContent = "Connecting to the model download…";
  }
  el("model").textContent = s.Phase === "download" ? "Cancel download" : "Download model";
  el("model").disabled = s.Busy && s.Phase !== "download";
  el("partial").disabled = s.Busy;
  el("file-type-options").disabled = s.Busy;
  el("auto").disabled = s.Busy || scanProfile?.effective.execution.mode === "preview_only";
  el("repair").disabled = s.Busy;

}

function renderScanLifecycleControls(s, hadWork, differentFolder, chosenOperation, partial, scanning) {
  const canResume = hadWork && s.Phase === "paused" && !s.Busy;
  show("start", !s.Busy);
  show("start-fresh", !s.Busy);
  el("start").textContent = "Start new scan";
  show("resume", canResume && !differentFolder);
  show("restore-run-folder", canResume && differentFolder);
  const runHint = s.StartingRoot ? "Finding files in " + s.StartingRoot
    : canResume && differentFolder ? "Use saved scan folder to return to the paused scan."
    : chosenOperation !== "duplicates" && partial ? "New scans will use built-in analysis."
    : "";
  el("run-hint").textContent = runHint;
  show("run-hint", Boolean(runHint));
  show("pause", s.Busy && scanning);
  show("undo", !s.Busy && (s.UndoLeft > 0 || s.Pending));
  el("undo").textContent = s.Phase === "undo-paused" ? "Resume undo" : "Undo changes";
  el("start").disabled = el("start-fresh").disabled = s.Busy || !el("folder").value.trim() ||
    el("scope-mode").value === "selected" && selectedFileExtensions().length === 0;
  el("pause").disabled = s.Pausing;
  el("pick").disabled = s.Busy;
  el("folder-renames").disabled = s.Busy;
  el("folder").disabled = s.Busy;
  el("exit").disabled = false;
  el("bridge").checked = s.BridgeEnabled;
  el("history").disabled = s.Busy || !(s.Jobs || []).length;

}

function renderScanProgress(s, partial, hadWork, startFailed, differentFolder, chosenOperation) {
  el("active-profile").textContent = scanProfile?.active ? "Profile: " + scanProfile.active : "";
  el("media-samples").disabled = s.Busy || partial;
  el("preview-only").disabled = s.Busy || scanProfile?.effective.execution.mode === "preview_only";
  el("auto").disabled ||= el("preview-only").checked;
  show("progress-panel", true);
  show("prototype-progress-summary", hadWork || Boolean(s.StartingRoot));
  show("progress", hadWork || Boolean(s.StartingRoot));
  el("run-folder").textContent = s.Root || "";
  el("run-scope").textContent = s.ScopeDescription || "";
  el("run-inventory").textContent = hadWork ? describeInventory(s) + (!s.Busy && s.OutcomeReason ? " · " + s.OutcomeReason : "")
    : s.StartingRoot ? "Finding files…" : startFailed ? "The scan could not start." : "Ready to start a scan";
  const moveReview = hadWork && !s.Busy && !differentFolder && chosenOperation === "move" && s.Operation === "move" && s.Proposed > 0;
  show("review-proposed-moves", moveReview);
  el("review-proposed-moves").textContent = "Review " + (s.Proposed || 0).toLocaleString() + " proposed " + (s.Proposed === 1 ? "move" : "moves");
  const clock = s.Clock;
  const hasTiming = Boolean(hadWork && clock?.Started && !clock.Started.startsWith("0001"));
  show("run-time", hasTiming);
  if (hasTiming) {
    el("run-time").textContent = duration(clock.ElapsedMS) + " elapsed · " + duration(clock.ActiveMS) + " active · " + duration(clock.PausedMS) + " paused";
  }
  const pc = s.Total ? Math.floor(100 * s.Done / s.Total) : (hadWork ? 100 : 0);
  el("count").textContent = `${(s.FilesAssessed || 0).toLocaleString()} files assessed · ${(s.FilesVerified || 0).toLocaleString()} names verified · ${(s.FilesSkipped || 0).toLocaleString()} skipped · ${(s.Proposed || 0).toLocaleString()} proposals`;
  el("percent").textContent = `${pc}%`;
  el("progress").value = pc;
  el("progress").max = 100;
  el("current").textContent = s.Current;
  for (const [id, key] of [["renamed","Renamed"],["moved","Moved"],["kept","Kept"],["flagged","Flagged"],["skipped","Skipped"],["metadata-done","MetadataDone"]]) {
    el(id).textContent = (s[key] || 0).toLocaleString();
  }
}

function renderFlaggedItems(s, hadWork) {
  show("export", hadWork && !s.Busy && s.Flagged > 0);
  show("flag-scroll", s.Flagged > 0);
  el("flag-description").textContent = s.Flagged > 0 ?
    (s.Flagged > 100 ? `Showing the last 100 of ${s.Flagged.toLocaleString()} items. The export includes all of them.` : "Automatic fixes and items that need a closer look.") :
    (s.Phase === "complete" ? "This run had no flagged items." : "Files that may need a look will appear here.");

  const flagKey = JSON.stringify(s.Flags);
  if (flagKey !== rowsKey) {
    rowsKey = flagKey;
    const rows = (s.Flags || []).map(row => {
      const tr = document.createElement("tr");
      for (const value of [row.Path, row.Status, row.Reason]) {
        const td = document.createElement("td");
        td.textContent = value;
        tr.append(td);
      }
      return tr;
    });
    el("rows").replaceChildren(...rows);
  }
}

function renderInspectionReports(s, hadWork, live) {
  const reports = hadWork && (!s.Busy || live || s.Phase === "reassessing");
  show("inspection-panel", reports);
  show("reassess-group", reports && !s.Busy && s.Done > 0);
  el("save-profile").disabled = el("restore-profile").disabled = s.Busy;
  const unknown = el("inspection-filter").value === "unknown";
  const newInspectionKey = reports && el("inspection-panel").open
    ? `${s.Job}:${s.Phase}:${s.Revision}:${s.Done}:${s.Renamed}:${s.Flagged}:${inspectionOffset}:${unknown}` : "";
  if (newInspectionKey !== inspectionKey) {
    inspectionKey = newInspectionKey;
    if (inspectionKey) {
      el("inspection-summary").textContent = "Loading…";
      show("inspection-scroll", false);
      show("inspection-pages", false);
      loadInspection(inspectionKey, inspectionOffset, unknown);
      loadDiagnostics(inspectionKey, s.Job);
    }
  }
}

function renderRunHistory(s) {
  const newHistoryKey = JSON.stringify(s.Jobs);
  if (newHistoryKey !== historyKey) {
    historyKey = newHistoryKey;
    const options = (s.Jobs || []).map(job => new Option(`${job.Date} · ${operationNames[job.Operation] || "File Renamer"} · ${job.Root}`,job.ID));
    el("history").replaceChildren(...(options.length ? options : [new Option("No runs yet","")]));
    for (const [id, defaultIndex] of [["compare-first",1],["compare-second",0]]) {
      const select=el(id), chosen=select.value;
      select.replaceChildren(...options.map(option=>option.cloneNode(true)));
      select.value=options.some(option=>option.value===chosen) ? chosen : options[defaultIndex]?.value || "";
    }
    if (options.length > 1 && el("compare-first").value === el("compare-second").value) {
      el("compare-second").value = options.find(option => option.value !== el("compare-first").value).value;
    }
  }
  if (s.Job) el("history").value = s.Job;
  show("comparison-panel", !s.Busy && (s.Jobs || []).length > 1);
}

// Section: State polling.

async function refresh() {
  if (closed) return;
  const serial = ++refreshSerial;
  try {
    const r = await fetch("/api/state", {cache:"no-store", signal:AbortSignal.timeout(5000)});
    if (!r.ok) throw new Error((await r.text()).trim());
    const state = await r.json();
    if (!closed && serial > renderedSerial) {renderedSerial=serial;render(state);}
  } catch (error) {
    if (!closed && serial > renderedSerial) {
      renderedSerial=serial;
      if (closing) {
        closed = true;
        const message = document.createElement("p");
        message.textContent = "Local File Renamer is closed. You can close this tab.";
        document.querySelector("main").replaceChildren(message);
      } else {
        notice(actionMessage || "Lost connection to the app. Refresh this tab or reopen Local File Renamer. " + error.message);
      }
    }
  }
}

// Section: Folder selection and scan configuration.

el("pick").onclick = async () => {
  el("pick").disabled = true;
  const result = await action("pick", {root:el("folder").value});
  if (result?.root && !closing && !closed) {
    folderEdited = true;
    el("folder").value = result.root;
  }
  if (latest.Phase) render(latest);
};
function selectedFileExtensions() {
  const checked = [...el("file-type-options").querySelectorAll("input[type=checkbox]:checked")].flatMap(input => input.value.split(","));
  const other = el("custom-extensions").value.trim().split(/[\s,;]+/).filter(Boolean).map(value => value.startsWith(".") ? value : "." + value);
  return [...new Set([...checked,...other].map(value => value.toLowerCase()))];
}
function profileRestrictions(scope) {
  if (!scope) return [];
  const restrictions = [];
  if (scope.include_extensions?.length) restrictions.push("Includes " + scope.include_extensions.join(", "));
  if (scope.exclude_extensions?.length) restrictions.push("Excludes " + scope.exclude_extensions.join(", "));
  for (const [key, label] of [["include_paths","included paths"], ["exclude_paths","excluded paths"], ["selected_paths","selected paths"]]) {
    if (scope[key]?.length) restrictions.push(scope[key].length + " " + label);
  }
  if (scope.min_bytes > 0) restrictions.push("Minimum " + scope.min_bytes + " bytes");
  if (scope.max_bytes > 0) restrictions.push("Maximum " + scope.max_bytes + " bytes");
  return restrictions;
}

function refreshFileTypeSelection() {
  const mode = el("scope-mode").value;
  const count = selectedFileExtensions().length;
  const restrictions = profileRestrictions(scanProfile?.effective.scope);
  show("file-type-options", mode === "selected");
  show("profile-scope", mode === "profile" && restrictions.length > 0);
  el("profile-scope").textContent = restrictions.join(" · ");
  el("file-type-summary").textContent = mode === "selected" ? "· " + count + " selected extensions" :
    mode === "profile" && restrictions.length ? "· Use profile scope" : "· All file types";
  if (latest.Phase) render(latest);
}

function resetScanOptions() {
  const settings = scanProfile?.effective;
  for (const input of el("file-type-options").querySelectorAll("input[type=checkbox]")) input.checked = false;
  el("custom-extensions").value = "";
  el("scope-mode").value = profileRestrictions(settings?.scope).length ? "profile" : "all";
  el("subfolders").checked = settings?.scope.subfolders ?? true;
  el("partial").checked = settings?.ai.model === "off" || settings?.ai.invoke === "never";
  el("auto").checked = settings?.execution.mode === "automatic";
  el("preview-only").checked = settings?.execution.mode === "preview_only";
  el("media-samples").checked = Boolean(settings?.evidence.media_samples);
  el("repair").checked = Boolean(settings?.execution.metadata);
  el("folder-renames").checked = Boolean(settings?.execution.folder_renames);
  refreshFileTypeSelection();
}

el("scope-mode").onchange = refreshFileTypeSelection;
el("file-type-options").addEventListener("change",refreshFileTypeSelection);
el("custom-extensions").addEventListener("input",refreshFileTypeSelection);

// Section: Scan launch and lifecycle controls.

async function submitScan(fresh) {
  if (pendingAction || pendingScan) return;
  const requestID = crypto.randomUUID();
  pendingScan = requestID;
  scanStartRejected = false;
  const result = await action("start", {
    requestID, fresh, root: el("folder").value, partial: el("partial").checked,
    folders: el("folder-renames").checked, auto: el("auto").checked, repair: el("repair").checked,
    operation: el("scan-operation").value, scopeMode: el("scope-mode").value,
    subfolders: el("subfolders").checked, includeExtensions: selectedFileExtensions(),
    previewOnly: el("preview-only").checked, mediaSamples: el("media-samples").checked
  });
  if (result) resetScanOptions();
  if (!result && pendingScan === requestID) {
    pendingScan = "";
    scanStartRejected = true;
    if (latest.Phase) render(latest);
  }
}

el("review-proposed-moves").onclick = () => selectView("review");
el("start").onclick = () => submitScan(false);
el("start-fresh").onclick = () => submitScan(true);
el("resume").onclick = () => action("resume",{id:latest.Job,partial:latest.Partial || el("partial").checked});
el("restore-run-folder").onclick = () => {
  folderEdited = false;
  el("folder").value = latest.Root;
  el("partial").checked = latest.Operation !== "duplicates" && latest.Partial;
  el("scan-operation").value = latest.Operation || "rename";
  render(latest);
};
el("pause").onclick = () => action("pause");
el("bridge").onchange = () => action("bridge", {enabled: el("bridge").checked});
el("model").onclick = () => action(latest.Phase === "download" ? "pause" : "model");
el("undo").onclick = () => {
  if (latest.Phase === "undo-paused" || window.confirm("Undo applied file moves, renames, and metadata repairs for this run?")) action("undo");
};
// Section: Batch approval and application.

async function applyAllProposals(filter, folder, description) {
  const {Job, Revision} = latest;
  if (!Job || latest.PreviewOnly) return;
  try {
    const q = new URLSearchParams({job:Job, revision:Revision, filter, folder});
    const response = await fetch("/api/apply-all?" + q, {cache:"no-store"});
    if (!response.ok) throw new Error((await response.text()).trim());
    const {count} = await response.json();
    if (!count) return notice("No pending changes match this review.");
    if (!window.confirm("Apply all " + count + " " + description + "?")) return;
    if (await action("apply-all",{id:Job,revision:Revision,group:filter,root:folder,count})) {
      selectedItems.clear();
    }
  } catch (error) {
    notice(error.message);
  }
}

el("apply").onclick = async () => {
  if (latest.Phase === "apply-paused") {
    action("apply",{id:latest.Job,revision:latest.Revision});
  } else if (selectedItems.size && window.confirm("Apply " + selectedItems.size + " selected changes?")) {
    const confirmed = [...selectedItems.keys()];
    if (await action("apply-selected",{id:latest.Job,indices:confirmed,revision:latest.Revision,rowRevisions:Object.fromEntries(selectedItems)})) {
      for (const index of confirmed) selectedItems.delete(index);
      if (currentView === "review") drawReview();
    }
  }
};
// Section: Pagination, history and application controls.

el("previous-page").onclick = () => {pageOffset=Math.max(0,pageOffset-100);updateReview(latest);};
el("next-page").onclick = () => {if (pageOffset+100<reviewTotal) {pageOffset+=100;updateReview(latest);}};
el("inspection-panel").ontoggle = () => render(latest);
el("inspection-filter").onchange = () => {inspectionOffset=0;render(latest);};
el("inspection-prev").onclick = () => {inspectionOffset=Math.max(0,inspectionOffset-100);render(latest);};
el("inspection-next").onclick = () => {inspectionOffset+=100;render(latest);};
el("data").onclick = () => action("open-data");
el("history").onchange = event => {
  folderEdited = false;
  scanStartRejected = false;
  action("select",{id:event.target.value});
};
el("exit").onclick = async () => {
  el("exit").disabled = true;
  await action("exit");
};
el("folder").oninput = () => {
  folderEdited = true;
  if (latest.Phase) render(latest);
};
el("preview-only").onchange = () => { if (latest.Phase) render(latest); };
el("partial").onchange = () => { if (latest.Phase) render(latest); };
el("scan-operation").onchange = () => { if (latest.Phase) render(latest); };

refresh();
loadSettings("",true);
setInterval(refresh,850);
