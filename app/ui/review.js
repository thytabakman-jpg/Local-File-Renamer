"use strict";

let currentView = "scan";
let reviewFilter = "all";
let inlineEdit;
let reviewRows = [];
let reviewTotal = 0;
let reviewIndex = -1;
let reviewRequest = 0;
let reviewCacheKey = "";
let detailKey = "";
let previewPage = 0;
let previewRequest;
let previewURL;
let recommendationKey = "";
let recommendationRequest;
let chosenRecommendation = "";
let recommendationOutcome = "";
let queuedNameJob = "";
const queuedNames = new Map();

// Section: View navigation and path utilities.

function selectView(view) {
  currentView = view;
  if (view !== "review") {
    resetRecommendations("");
    closeInlineName();
  }
  for (const name of ["scan", "review", "history", "settings", "chat"]) show(name + "-view", name === view);
  for (const button of document.querySelectorAll("[data-view]")) button.setAttribute("aria-pressed", String(button.dataset.view === view));
  if (view === "chat") {
    const folder = el("folder").value.trim();
    el("chat-folder").textContent = folder ? "Selected scan folder: " + folder : "Choose a folder in New scan or give its full path here.";
    el("chat-input").focus();
  }
  if (view === "review") updateReview(latest);
  if (view === "history") refreshTestHistory();
}

function leaf(path) { return path?.split(/[\\/]/).at(-1) || ""; }
function parentFolder(path) {
  const prefix = path.slice(0, path.length - leaf(path).length);
  return /^(?:[A-Za-z]:)?[\\/]$/.test(prefix) ? prefix : prefix.replace(/[\\/]+$/, "");
}

// Section: Review results and list rendering.

function updateReview(state) {
  if (currentView !== "review" || !state.Phase) return;
  show("review-inventory", Boolean(state.Job && !state.StartingRoot));
  el("review-inventory").textContent = state.Job ? describeInventory(state) + " · " + state.ScopeDescription +
    (!state.Busy && state.OutcomeReason ? " · " + state.OutcomeReason : "") : "";
  if (state.StartingRoot) {
    show("operation-review", false);
    show("filename-review", true);
    el("review-context").textContent = "Finding files in " + state.StartingRoot;
    reviewCacheKey = "";
    ++reviewRequest;
    reviewRows = [];
    reviewTotal = 0;
    drawReview();
    show("review-pause", true);
    return;
  }
  const specialised = state.Operation === "move" || state.Operation === "duplicates";
  show("operation-review", specialised);
  show("filename-review", !specialised);
  if (specialised) {
    updateOperationReview(state);
    return;
  }
  const phase = phases[state.Phase] || state.Phase;
  el("review-context").textContent = state.Job ?
    [leaf(state.Root), phase, state.Partial ? "Local AI off" : "Local AI on", state.PreviewOnly ? "Preview only" : state.ExecutionMode === "automatic" ? "Automatic" : "Review before applying"].join(" · ") : "Start a scan to review files.";
  const revisionKey = [state.Job, state.Revision, state.Phase, state.Done, state.Proposed, state.Drafts, reviewFilter, pageOffset].join(":");
  if (revisionKey !== reviewCacheKey) {
    reviewCacheKey = revisionKey;
    ++reviewRequest;
    const active = canReview(state) || state.Busy && ["files","folders","starting"].includes(state.Phase);
    if (!state.Job || reviewFilter === "proposed" && (!active || !state.Proposed && !state.Drafts)) {
      reviewRows = [];
      reviewTotal = 0;
      drawReview();
    } else {
      fetchReviewRows(state, active);
    }
  }
  renderReviewControls(state);
}

async function fetchReviewRows(state, active) {
  const serial = ++reviewRequest;
  const proposed = reviewFilter === "proposed";
  const query = new URLSearchParams({job:state.Job, offset:pageOffset, filter:reviewFilter});
  try {
    const response = await fetch((proposed ? "/api/preview?" : "/api/inspection?") + query, {cache:"no-store", signal:AbortSignal.timeout(5000)});
    if (!response.ok) throw new Error((await response.text()).trim());
    const page = await response.json();
    if (!proposed && page.Job !== state.Job || serial !== reviewRequest || currentView !== "review" || state.Job !== latest.Job || state.Revision !== latest.Revision) return;
    reviewRows = proposed ? (page || []) : page.Rows.map(row => ({
      AutomaticFrom:row.AutomaticFrom, AutomaticTo:row.AutomaticTo, AutomaticReason:row.AutomaticReason, AutomaticUndone:row.AutomaticUndone,
      Index:row.Index, Revision:row.Revision, From:row.Path, To:row.Proposed, Decision:row.Decision, Reason:row.Reason, Note:row.Note, Inspection:row.Inspection,
      ReviewActions:row.ReviewActions, Refinement:row.Refinement, Metadata:row.Metadata, Retry:row.Outcome === "Rename failed"
    }));
    reviewTotal = proposed ? state.Proposed + (active && state.Busy ? state.Drafts || 0 : 0) : page.Total;
    drawReview();
  } catch (err) {
    if (serial === reviewRequest && currentView === "review") {
      reviewRows = [];
      reviewTotal = 0;
      drawReview();
      el("preview-summary").textContent = err.message;
    }
  }
}

function drawReview() {
  if (!reviewRows.some(row => row.Index === reviewIndex)) reviewIndex = reviewRows[0]?.Index ?? -1;
  const list = el("proposals");
  const previous = document.activeElement?.closest("[data-review-index]");
  el("proposals").replaceChildren(...reviewRows.map(row => {
    const li = document.createElement("li");
    li.dataset.reviewIndex = row.Index;
    if (row.Index === reviewIndex) li.classList.add("focused-file");
    if (rowSelectable(row, latest)) {
      const check = document.createElement("input");
      check.type = "checkbox";
      if (selectedItems.has(row.Index) && selectedItems.get(row.Index) !== row.Revision) selectedItems.delete(row.Index);
      check.checked = selectedItems.has(row.Index);

      check.setAttribute("aria-label", "Select " + leaf(row.From));
      check.onchange = () => {
        if (check.checked) selectedItems.set(row.Index,row.Revision);
        else selectedItems.delete(row.Index);
        renderReviewControls(latest);
      };
      li.append(check);
    } else {
      const spacer = document.createElement("span");
      spacer.className = "prototype-check-space";
      li.append(spacer);
    }
    const button = document.createElement("button");
    button.type = "button";
    button.className = "file-list-button";
    button.setAttribute("aria-pressed", String(row.Index === reviewIndex));
    const extension = leaf(row.From).split(".").at(-1).toUpperCase();
    const previewable = ["JPG","JPEG","JFIF","JPE","PNG","GIF","BMP","TIFF","TIF","WEBP"].includes(extension);
    const badge = document.createElement("span");
    badge.className = "file-type-icon";
    badge.textContent = extension.length <= 5 ? extension : "FILE";
    let visual = badge;
    if (previewable) {
      const img = document.createElement("img");
      img.className = "prototype-image-thumb";
      img.alt = "";
      img.loading = "lazy";
      img.onerror = () => img.replaceWith(badge);
      img.src = "/api/file-preview?" + new URLSearchParams({job:latest.Job,index:row.Index,thumbnail:"1"});
      visual = img;
    }
    const names = document.createElement("span");
    names.className = "prototype-filename";
    const current = document.createElement("span");
    current.className = "prototype-current-name";
    const name = document.createElement("span");
    name.textContent = leaf(reviewFilter === "auto" ? row.AutomaticFrom : row.From);
    const category = document.createElement("small");
    category.className = "prototype-row-state";
    category.textContent = row.Draft ? "Draft" : row.AutomaticFrom
      ? (row.AutomaticUndone ? "Automatic change undone" : row.Refinement ? "Auto-renamed · Needs review" : "Auto-renamed")
      : row.Retry ? "Rename failed" : row.Decision || "Proposed";
    current.append(name, category);
    const proposal = document.createElement("span");
    proposal.className = "prototype-proposed-name";
    proposal.textContent = reviewFilter === "auto" ? leaf(row.AutomaticTo) :
      row.To ? leaf(row.To) : row.ReviewActions ? "Review filename" : "";
    names.append(current, proposal);
    button.title = row.From;
    button.append(visual,names);
    button.onclick = () => {
      reviewIndex = row.Index;
      drawReview();
      list.querySelector('[data-review-index="' + row.Index + '"] button')?.focus();
    };
    li.append(button);
    return li;
  }));
  if (previous && list.contains(document.activeElement) === false) {
    list.querySelector('[data-review-index="' + previous.dataset.reviewIndex + '"] button')?.focus();
  }
  show("review-selected", reviewIndex >= 0);
  show("review-empty", reviewIndex < 0);
  if (reviewIndex >= 0) renderFileDetail(reviewRows.find(row => row.Index === reviewIndex));
  else {
    el("review-empty").textContent = latest.Job ? "No matching files to show." : "Select New scan to choose a folder.";
    clearFilePreview();
  }
  renderReviewControls(latest);
}

// Section: Selection eligibility and review controls.

function rowSelectable(row, state) {
  return canReview(state) && !state.PreviewOnly && !row.Draft && Boolean(row.To || row.Metadata) &&
    reviewFilter !== "auto";
}

function reviewRowKey(state, row) {
  return [state.Job, row.Index, row.Revision, row.From, row.To].join(":");
}

function renderReviewControls(state) {
  const pending = state.Phase === "apply-paused";
  const row = reviewRows.find(row => row.Index === reviewIndex);
  const ready = row && rowSelectable(row, state);
  const flagged = row && row.ReviewActions && !state.PreviewOnly && !state.Pending && ["all","flagged","auto"].includes(reviewFilter);
  const editable = ready || flagged;
  show("flagged-names", Boolean(flagged));
  show("delete-flagged-file", Boolean(flagged && !state.Busy && !row.Refinement));
  resetRecommendations(flagged ? reviewRowKey(state,row) : "");
  show("apply", !state.PreviewOnly && (pending || reviewRows.some(item => rowSelectable(item,state))));
  el("apply").disabled = !pending && selectedItems.size === 0;
  const all = !pending && canReview(state) && !state.PreviewOnly && state.Proposed > 0 &&
    ["all","proposed","flagged","kept"].includes(reviewFilter);
  show("apply-all-renames", all);
  el("apply-all-renames").textContent = reviewFilter === "flagged" ? "Apply all flagged proposals" :
    reviewFilter === "kept" ? "Apply all proposals for kept files" : "Apply all proposals";
  updateQueuedNames(state);
  el("apply").textContent = pending ? "Resume applying" : "Apply selected (" + selectedItems.size + ")";
  show("review-pause", state.Busy && ["indexing","starting","files","folders","reassessing","applying","metadata"].includes(state.Phase));
  el("review-pause").disabled = state.Pausing;
  show("review-undo", !state.Busy && (state.UndoLeft > 0 || state.Pending));
  show("edit-filename", Boolean(editable));
  if (inlineEdit && (!row || inlineEdit.key !== reviewRowKey(state,row))) closeInlineName();
  show("apply-current", Boolean(ready || inlineEdit));
  el("prototype-selection").textContent = selectedItems.size + " selected";
  el("prototype-current-column-title").textContent = reviewFilter === "auto" ? "Original filename" : "Current filename";
  el("prototype-proposed-column-title").textContent = reviewFilter === "auto" ? "Automatically changed to" : "Proposed change";
  el("apply-current").textContent = inlineEdit ? "Apply name" : row?.Retry || row?.Note?.includes("Rename failed:") ? "Retry change" : "Apply this change";
  el("edit-filename").textContent = flagged && !ready ? "Enter a name" : "Edit proposed name";
  show("keep-filename", Boolean(flagged && !row.Refinement));
  show("location-controls", Boolean(editable && row.To && parentFolder(row.From) !== parentFolder(row.To)));
  show("page-controls", reviewTotal > 100);
  el("previous-page").disabled = pageOffset === 0;
  el("next-page").disabled = pageOffset + 100 >= reviewTotal;
  el("preview-range").textContent = (pageOffset + 1) + "–" + Math.min(pageOffset + 100, reviewTotal) + " of " + reviewTotal;
  el("preview-summary").textContent = state.ApprovedWaiting ? state.ApprovedWaiting + " approved · waiting to apply" : state.Busy ? (state.Done || 0) + " of " + (state.Total || 0) + " items assessed" :
    reviewFilter === "proposed" ? (state.PreviewOnly ? "This run uses preview-only permissions." : (state.Proposed || 0) + " proposals awaiting review.") : reviewTotal + " files";
}

// Section: File details and location tree.

function renderFileDetail(row) {
  const automatic = reviewFilter === "auto" && Boolean(row.AutomaticFrom);
  el("prototype-current-label").textContent = automatic ? "Original filename" : "Current filename";
  el("prototype-proposed-label").textContent = automatic ? "Automatically changed to" : "Proposed filename";
  el("current-filename").textContent = leaf(automatic ? row.AutomaticFrom : row.From);
  const keyForRecommendations = reviewRowKey(latest,row);
  const chosen = reviewFilter === "flagged" && recommendationKey === keyForRecommendations && chosenRecommendation;
  el("proposed-filename").textContent = chosen || (automatic && leaf(row.AutomaticTo)) || (row.To && leaf(row.To) !== leaf(row.From) ? leaf(row.To) : "Unchanged");
  el("review-reason").textContent = [row.Decision, row.Reason].filter(Boolean).join(" · ");
  el("review-note").textContent = row.Note || "";
  show("prototype-auto-evidence", Boolean(row.AutomaticFrom));
  el("prototype-auto-evidence").textContent = row.AutomaticFrom
    ? [row.AutomaticUndone ? "Automatic change undone" : "Automatically renamed", row.AutomaticReason].filter(Boolean).join(" · ") : "";
  const fields = ["Title", "Author"].filter(key => row.Metadata?.[key]);
  show("metadata-unchanged", fields.length === 0);
  show("metadata-diff", fields.length > 0);
  for (const [id, prefix] of [["current-metadata","Old"],["proposed-metadata",""]]) {
    el(id).textContent = fields.map(key => key + ": " + (row.Metadata[prefix+key] || "(blank)")).join("\n");
  }
  const from = parentFolder(row.From), to = row.To ? parentFolder(row.To) : from;
  el("prototype-location-label").textContent = from === to ? "Current folder" : "Proposed folder";
  el("prototype-location-path").textContent = to;
  el("location-from").textContent = from;
  el("location-to").textContent = to;
  renderLocationTree(from,to);
  const key = latest.Job + ":" + row.Index + ":" + row.From;
  if (detailKey !== key) {
    detailKey = key;
    previewPage = 0;
    selectDetailTab(false);
    loadFilePreview(row);
  }
}

function selectDetailTab(location) {
  show("file-location", location);
  show("file-details", !location);
  el("location-tab").setAttribute("aria-pressed", String(location));
  el("file-details-tab").setAttribute("aria-pressed", String(!location));
}

function pathParts(path) {
  const normalized = path.replaceAll("\\","/");
  const root = normalized.startsWith("//") ? "//" : normalized.startsWith("/") ? "/" : "";
  return [...(root ? [root] : []), ...normalized.split("/").filter(Boolean)];
}

function renderLocationTree(from, to, target = "location-tree") {
  const left = pathParts(from), right = pathParts(to);
  const windows = /^[A-Za-z]:|^[\\]{2}/.test(from);
  const equal = (a,b) => windows ? a.toLowerCase() === b.toLowerCase() : a === b;
  let common = 0;
  while (common < Math.min(left.length,right.length) && equal(left[common],right[common])) common++;
  const node = (title, caption, css) => {
    const div = document.createElement("div");
    div.className = "folder-node " + css;
    const label = document.createElement("strong");
    label.textContent = title;
    div.append(label);
    if (caption) {
      const small = document.createElement("small");
      small.textContent = caption;
      div.append(small);
    }
    return div;
  };
  const tree = el(target);
  tree.replaceChildren();
  if (from === to) {
    tree.append(node(left.at(-1) || from, "Current folder", "shared-folder"));
    return;
  }
  if (common) tree.append(node(left[common-1],"Nearest shared folder","shared-folder"));
  const branches = document.createElement("div");
  branches.className = "folder-branches" + (common ? " connected" : "");
  for (const [parts, role] of [[left, "Current folder"],[right, "Proposed destination"]]) {
    const branch = document.createElement("div");
    branch.className = "folder-branch" + (role === "Proposed destination" ? " destination" : "");
    const children = parts.slice(common);
    if (children.length) {
      children.forEach((part,index) => branch.append(node(part,index === children.length-1 ? role : "", index === children.length-1 ? "endpoint" : "ancestor")));
    } else {
      branch.append(node(parts.at(-1),role,"endpoint"));
    }
    branches.append(branch);
  }
  tree.append(branches);
}

// Section: File preview loading and rendering.

function clearFilePreview() {
  previewRequest?.abort();
  if (previewURL) URL.revokeObjectURL(previewURL);
  previewURL = null;
  detailKey = "";
}

async function loadFilePreview(row, page = 0) {
  if (!row) return;
  const key = detailKey;
  previewRequest?.abort();
  const request = new AbortController();
  previewRequest = request;
  const target = el("file-visual");
  target.textContent = "Loading preview…";
  show("preview-pages", false);
  show("open-pdf", false);
  const ext = leaf(row.From).split(".").at(-1).toLowerCase();
  try {
    const query = new URLSearchParams({job:latest.Job, index:row.Index, page});
    const response = await fetch("/api/file-preview?" + query, {signal:request.signal,cache:"no-store"});
    if (!response.ok) throw new Error((await response.text()).trim() || "Preview unavailable for this file.");
    if (detailKey !== key || request.signal.aborted) return;
    if (response.headers.get("Content-Type")?.includes("image/jpeg")) {
      const image = await response.blob();
      if (detailKey !== key || request.signal.aborted) return;
      if (previewURL) URL.revokeObjectURL(previewURL);
      previewURL = URL.createObjectURL(image);
      const img = document.createElement("img");
      img.alt = ext === "pdf" ? "PDF page " + (page + 1) : "Preview of " + leaf(row.From);
      img.src = previewURL;
      target.replaceChildren(img);
      show("preview-pages", ext === "pdf");
      el("file-prev").disabled = page === 0;
      el("file-next").disabled = false;
      el("file-page").textContent = "Page " + (page + 1);
    } else {
      const documentPreview = await response.json();
      if (detailKey !== key || request.signal.aborted) return;
      const caption = document.createElement("small");
      caption.textContent = documentPreview.Label;
      const paper = document.createElement("div");
      paper.className = "preview-paper";
      for (const paragraph of documentPreview.Paragraphs) {
        const line = document.createElement(paragraph.Style === "title" ? "h3" : paragraph.Style === "heading" ? "h4" : "p");
        line.textContent = paragraph.Text;
        paper.append(line);
      }
      target.replaceChildren(caption,paper);
    }
    previewPage = page;
  } catch (err) {
    if (detailKey === key && !request.signal.aborted) {
      target.textContent = err.message;
      show("open-pdf", ext === "pdf");
      if (page > 0 && ext === "pdf") {
        previewPage = page;
        el("file-page").textContent = "Page " + (page + 1);
        show("preview-pages",true);
        el("file-prev").disabled = false;
        el("file-next").disabled = true;
      }
    }
  }
}

for (const button of document.querySelectorAll("[data-view]")) button.onclick = () => selectView(button.dataset.view);
for (const button of document.querySelectorAll("[data-review-filter]")) {
  button.onclick = () => {
    reviewFilter = button.dataset.reviewFilter;
    closeInlineName();
    selectedItems.clear();
    pageOffset = 0;
    for (const other of document.querySelectorAll("[data-review-filter]")) other.setAttribute("aria-pressed", String(other === button));
    updateReview(latest);
  };
}
// Section: Name recommendations and selection.

const activeReviewRow = () => reviewRows.find(row => row.Index === reviewIndex);
function resetRecommendations(key) {
  if (key === recommendationKey) return;
  recommendationRequest?.abort();
  recommendationRequest = null;
  recommendationKey = key;
  chosenRecommendation = "";
  recommendationOutcome = "";
  el("recommend-names").disabled = false;
  el("use-recommended-name").disabled = false;
  el("recommended-names-status").textContent = "";
  el("recommended-names-list").replaceChildren();
  show("save-recommended-name",false);
}

el("recommend-names").onclick = async () => {
  const row = activeReviewRow();
  const key = row && reviewRowKey(latest,row);
  if (!recommendationKey || key !== recommendationKey || recommendationRequest) return;
  const request = new AbortController();
  recommendationRequest = request;
  el("recommend-names").disabled = true;
  el("recommended-names-status").textContent = "Finding names from this file…";
  recommendationOutcome = "Request pending";
  const query = new URLSearchParams({job:latest.Job,index:row.Index,revision:row.Revision});
  try {
    const response = await fetch("/api/recommended-names?" + query, {signal:request.signal,cache:"no-store"});
    if (!response.ok) throw new Error((await response.text()).trim());
    const names = await response.json();
    if (request !== recommendationRequest) return;
    chosenRecommendation = "";
    show("save-recommended-name",false);
    el("proposed-filename").textContent = row.To ? leaf(row.To) : "Unchanged";
    el("recommended-names-status").textContent = names.length ? "" : "The file did not provide enough evidence for another name.";
    recommendationOutcome = names.length ? names.length + " names returned" : "Request completed with zero names";
    const list = el("recommended-names-list");
    list.replaceChildren(...names.map(name => {
      const option = document.createElement("button");
      option.type = "button";
      option.className = "secondary";
      option.textContent = name;
      option.setAttribute("aria-pressed","false");
      option.onclick = () => {
        chosenRecommendation = name;
        for (const button of list.children) button.setAttribute("aria-pressed",String(button === option));
        el("proposed-filename").textContent = name;
        show("save-recommended-name",true);
      };
      return option;
    }));
  } catch (err) {
    if (request === recommendationRequest) {
      recommendationOutcome = "Request failed";
      el("recommended-names-status").textContent = err.message;
    }
  } finally {
    if (request === recommendationRequest) {
      recommendationRequest = null;
      el("recommend-names").disabled = false;
    }
  }
};

// Section: Queued name approvals and recycling.

function updateQueuedNames(state) {
  if (queuedNameJob !== state.Job) {
    queuedNames.clear();
    queuedNameJob = state.Job;
  }
  const count = queuedNames.size;
  show("apply-all-names", count > 0 && canReview(state) && !state.PreviewOnly);
  show("clear-queued-names", count > 0);
  el("apply-all-names").textContent = "Apply all queued names (" + count + ")";
}

el("apply-all-renames").onclick = () => applyAllProposals(reviewFilter, "", reviewFilter === "flagged" ?
  "pending flagged proposals" : reviewFilter === "kept" ? "pending proposals for kept files" : "pending proposals across all pages");

el("queue-recommended-name").onclick = () => {
  const row = activeReviewRow();
  if (!row || reviewRowKey(latest,row) !== recommendationKey || !chosenRecommendation) return;
  queuedNames.set(row.Index, {index:row.Index, revision:row.Revision, name:chosenRecommendation});
  el("recommended-names-status").textContent = "Name queued for this file.";
  updateQueuedNames(latest);
};
el("clear-queued-names").onclick = () => {
  queuedNames.clear();
  updateQueuedNames(latest);
};
el("apply-all-names").onclick = async () => {
  const id = latest.Job;
  const names = [...queuedNames.values()];
  if (!names.length || !window.confirm("Apply all " + names.length + " queued names?")) return;
  if (await action("apply-chosen-names", {id, chosenNames:names})) {
    for (const name of names) queuedNames.delete(name.index);
    updateQueuedNames(latest);
  }
};

el("use-recommended-name").onclick = async () => {
  const row = activeReviewRow();
  const key = row && reviewRowKey(latest,row);
  if (!recommendationKey || key !== recommendationKey || !chosenRecommendation) return;
  const job = latest.Job;
  el("use-recommended-name").disabled = true;
  const saved = await action("use-review-name",{id:job,index:row.Index,revision:row.Revision,filename:chosenRecommendation});
  el("use-recommended-name").disabled = false;
  if (saved) {
    queuedNames.delete(row.Index);
    updateQueuedNames(latest);
  }
  if (saved && latest.Job === job && currentView === "review" && reviewFilter === "flagged") {
    reviewIndex = row.Index;
    resetRecommendations("");
    updateReview(latest);
  }
};

el("delete-flagged-file").onclick = async () => {
  const row = activeReviewRow();
  if (!row || el("delete-flagged-file").hidden ||
      !window.confirm("Move “" + leaf(row.From) + "” to the Windows Recycle Bin?")) return;
  if (await action("recycle-file",{id:latest.Job,indices:[row.Index],revision:row.Revision})) {
    actionMessage = "Moved " + leaf(row.From) + " to the Recycle Bin.";
    notice(actionMessage);
  }
};
// Section: Review lifecycle and detail navigation.

el("review-pause").onclick = () => action("pause");
el("review-undo").onclick = () => el("undo").click();
el("history-review").onclick = () => selectView("review");
el("file-details-tab").onclick = () => selectDetailTab(false);
el("location-tab").onclick = () => selectDetailTab(true);
el("file-prev").onclick = () => loadFilePreview(activeReviewRow(), Math.max(0,previewPage-1));
el("file-next").onclick = () => loadFilePreview(activeReviewRow(), previewPage+1);
// Section: Inline filename editing and application.

function closeInlineName() {
  inlineEdit = undefined;
  show("prototype-inline-edit", false);
}

function openInlineName(row) {
  inlineEdit = {key:reviewRowKey(latest,row)};
  el("prototype-inline-filename").value = leaf(row.To || row.From);
  show("prototype-inline-edit", true);
  el("prototype-inline-filename").focus();
  el("prototype-inline-filename").select();
  renderReviewControls(latest);
}

el("edit-filename").onclick = () => {
  const row = activeReviewRow();
  if (row) openInlineName(row);
};
el("prototype-cancel-edit").onclick = () => {
  closeInlineName();
  renderReviewControls(latest);
};
el("prototype-inline-filename").onkeydown = event => {
  if (event.key === "Enter") el("apply-current").click();
  if (event.key === "Escape") el("prototype-cancel-edit").click();
};
el("apply-current").onclick = async () => {
  const row = activeReviewRow();
  if (!row) return;
  const fields = {id:latest.Job,index:row.Index,revision:row.Revision};
  const filename = inlineEdit && el("prototype-inline-filename").value.trim();
  if (inlineEdit && !filename) return notice("Enter a filename to apply.");
  const result = filename ? await action("use-review-name", {...fields,filename}) : await action("apply-one", fields);
  if (result) {
    selectedItems.delete(row.Index);
    closeInlineName();
  }
  renderReviewControls(latest);
};
// Section: Location and individual review actions.

el("prototype-view-location").onclick = () => selectDetailTab(true);
el("keep-location").onclick = () => changeReviewLocation("");
el("choose-location").onclick = async () => {
  const row = activeReviewRow();
  if (!row) return;
  const picked = await action("pick",{root:parentFolder(row.To || row.From)});
  if (picked?.root && activeReviewRow()?.Index === row.Index) changeReviewLocation(picked.root);
};
function changeReviewLocation(root) {
  const row = activeReviewRow();
  if (row) action("change-location",{root,id:latest.Job,index:row.Index,revision:row.Revision}).then(result => {
    if (result && !root && leaf(row.From) === leaf(row.To)) selectedItems.delete(row.Index);
    if (result) renderReviewControls(latest);
  });
}

el("scan-settings-link").onclick = () => {
  selectView("settings");
  displaySettingsCategory("scope");
  el("next-scan-settings").open = true;
  el("next-scan-settings").scrollIntoView({block:"start"});
};
el("keep-filename").onclick = () => {
  const row = activeReviewRow();
  if (row) action("use-review-name", {id:latest.Job,index:row.Index,revision:row.Revision,filename:leaf(row.From)});
};
el("open-pdf").onclick = () => {
  const row = activeReviewRow();
  if (row) action("open-pdf", {id:latest.Job,index:row.Index});
};

let bugContext;
// Section: Diagnostic report generation and controls.

function drawBugReport() {
  const {state, row, tab, recommendation, visibleApply, selectable, error} = bugContext;
  const paths = el("bug-paths").checked;
  const lines = [
    "Local File Renamer " + el("app-version").textContent,
    "Run ID: " + (state.Job || "No run selected"),
    "Tab: " + tab,
    "Phase: " + state.Phase,
    "Settings: " + (state.ScopeDescription || "") + " · " + (state.Partial ? "Built-in checks" : "Local AI enabled") +
      " · " + (state.ExecutionMode || (state.PreviewOnly ? "Preview only" : "Review")),
    "Progress: " + (state.FilesAssessed || 0) + " assessed / " + (state.FileTotal || 0) + " files",
    "Proposed: " + (state.Proposed || 0) + " · Applied: " + (state.Renamed || 0),
    "Apply selected control: " + (visibleApply ? "visible" : "hidden"),
    "Selected row has a checkbox: " + (selectable ? "yes" : "no"),
    "Selected row: " + (row ? [row.Index, row.Decision, row.Draft ? "draft" : "assessed", row.To ? "has proposal" : "no proposal"].join(" · ") : "none"),
    "Recommendation result: " + (recommendation || "No request recorded for this row"),
    "Recent error: " + (error ? (paths ? error : "An error was reported. Enable filenames and paths to include details.") : "None shown"),
    "Expected result: " + el("bug-expected").value.trim()
  ];
  if (paths && state.Root) lines.push("Folder: " + state.Root);
  if (paths && row) lines.push("Current path: " + row.From, "Proposed path: " + (row.To || "None"));
  el("bug-report-text").value = lines.join("\n");
}

function openBugReport() {
  const row = currentView === "review" ? activeReviewRow() : null;
  bugContext = {state:latest, row, tab:currentView === "review" ? currentView + " / " + reviewFilter : currentView,
    recommendation:recommendationOutcome, visibleApply:currentView === "review" && !el("apply").hidden,
    selectable:Boolean(row && rowSelectable(row,latest)),
    error:(recommendationOutcome === "Request failed" && el("recommended-names-status").textContent) ||
      actionMessage || latest.Message || row?.Note};
  el("bug-expected").value = "";
  el("bug-paths").checked = false;
  el("bug-copy-status").textContent = "";
  drawBugReport();
  el("bug-report-dialog").showModal();
}
el("review-bug-report").onclick = openBugReport;
el("operation-bug-report").onclick = openBugReport;
el("history-bug-report").onclick = openBugReport;
el("bug-expected").oninput = drawBugReport;
el("bug-paths").onchange = drawBugReport;
el("bug-copy").onclick = async () => {
  try {
    await navigator.clipboard.writeText(el("bug-report-text").value);
    el("bug-copy-status").textContent = "Report copied.";
  } catch {
    el("bug-report-text").select();
    el("bug-copy-status").textContent = "Press Ctrl+C to copy the selected report.";
  }
};
