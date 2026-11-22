"use strict";

let currentView = "scan";
let reviewFilter = "proposed";
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

function selectView(view) {
  if (view === "scan" && currentView !== "scan") resetScanOptions();
  currentView = view;
  if (view !== "review") resetRecommendations("");
  for (const name of ["scan", "review", "history", "settings", "chat"]) show(name + "-view", name === view);
  for (const button of document.querySelectorAll("[data-view]")) button.setAttribute("aria-pressed", String(button.dataset.view === view));
  if (view === "chat") {
    const folder = el("folder").value.trim();
    el("chat-folder").textContent = folder ? "Selected scan folder: " + folder : "Choose a folder in New scan or give its full path here.";
    el("chat-input").focus();
  }
  if (view === "review") updateReview(latest);
}

function leaf(path) { return path?.split(/[\\/]/).at(-1) || ""; }
function parentFolder(path) {
  const prefix = path.slice(0, path.length - leaf(path).length);
  return /^(?:[A-Za-z]:)?[\\/]$/.test(prefix) ? prefix : prefix.replace(/[\\/]+$/, "");
}

function updateReview(state) {
  if (currentView !== "review" || !state.Phase) return;
  show("review-inventory", Boolean(state.Job && !state.StartingRoot));
  el("review-inventory").textContent = state.Job ? describeInventory(state) + " · " + state.ScopeDescription : "";
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
    [leaf(state.Root), phase, state.Partial ? "Local AI off" : "Local AI on", state.PreviewOnly ? "Preview only" : "Review before applying"].join(" · ") : "Start a scan to review files.";
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
  const query = new URLSearchParams({offset:pageOffset, filter:reviewFilter});
  try {
    const response = await fetch((proposed ? "/api/preview?" : "/api/inspection?") + query, {cache:"no-store", signal:AbortSignal.timeout(5000)});
    if (!response.ok) throw new Error((await response.text()).trim());
    const page = await response.json();
    if (serial !== reviewRequest || currentView !== "review" || state.Job !== latest.Job || state.Revision !== latest.Revision) return;
    reviewRows = proposed ? (page || []) : page.Rows.map(row => ({
      Index:row.Index, From:row.Path, To:row.Proposed, Decision:row.Decision, Reason:row.Reason, Note:row.Note, Inspection:row.Inspection,
      ReviewActions:row.ReviewActions
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
    if (reviewFilter === "proposed") {
      const check = document.createElement("input");
      check.type = "checkbox";
      check.checked = selectedItems.has(row.Index);
      check.disabled = !canReview(latest) || latest.PreviewOnly || row.Draft;
      check.setAttribute("aria-label", "Select " + leaf(row.From));
      check.onchange = () => {
        if (check.checked) selectedItems.add(row.Index);
        else selectedItems.delete(row.Index);
        renderReviewControls(latest);
      };
      li.append(check);
    }
    const button = document.createElement("button");
    button.type = "button";
    button.className = "file-list-button";
    button.setAttribute("aria-pressed", String(row.Index === reviewIndex));
    const type = document.createElement("span");
    const extension = leaf(row.From).split(".").at(-1).toUpperCase();
    type.className = "file-type-icon";
    type.textContent = extension.length <= 5 ? extension : "FILE";
    const name = document.createElement("span");
    name.textContent = leaf(row.From);
    button.title = row.From;
    button.append(type,name);
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

function renderReviewControls(state) {
  const pending = state.Phase === "apply-paused";
  const row = reviewRows.find(row => row.Index === reviewIndex);
  const editable = row && canReview(state) && !state.PreviewOnly && !row.Draft && reviewFilter === "proposed";
  const flagged = row && row.ReviewActions && !state.Busy && !state.PreviewOnly && !state.Pending && reviewFilter === "flagged";
  show("flagged-names", Boolean(flagged));
  show("delete-flagged-file", Boolean(flagged));
  resetRecommendations(flagged ? [state.Job,state.Revision,row.Index].join(":") : "");
  show("apply", !state.PreviewOnly && !state.Busy && (pending || canReview(state) && state.Proposed > 0));
  el("apply").disabled = !pending && selectedItems.size === 0;
  el("apply").textContent = pending ? "Resume applying" : "Apply selected (" + selectedItems.size + ")";
  show("review-pause", state.Busy && ["indexing","starting","files","folders","reassessing","applying","metadata"].includes(state.Phase));
  el("review-pause").disabled = state.Pausing;
  show("review-undo", !state.Busy && (state.UndoLeft > 0 || state.Pending));
  show("edit-filename", Boolean(editable));
  show("apply-current", Boolean(editable));
  show("location-controls", Boolean(editable && row.To && parentFolder(row.From) !== parentFolder(row.To)));
  show("page-controls", reviewTotal > 100);
  el("previous-page").disabled = pageOffset === 0;
  el("next-page").disabled = pageOffset + 100 >= reviewTotal;
  el("preview-range").textContent = (pageOffset + 1) + "–" + Math.min(pageOffset + 100, reviewTotal) + " of " + reviewTotal;
  el("preview-summary").textContent = state.Busy ? (state.Done || 0) + " of " + (state.Total || 0) + " items assessed" :
    reviewFilter === "proposed" ? (state.PreviewOnly ? "This run uses preview-only permissions." : (state.Proposed || 0) + " proposals awaiting review.") : reviewTotal + " files";
}

function renderFileDetail(row) {
  el("current-filename").textContent = leaf(row.From);
  const keyForRecommendations = [latest.Job,latest.Revision,row.Index].join(":");
  const chosen = reviewFilter === "flagged" && recommendationKey === keyForRecommendations && chosenRecommendation;
  el("proposed-filename").textContent = chosen || (row.To && leaf(row.To) !== leaf(row.From) ? leaf(row.To) : "Unchanged");
  el("review-reason").textContent = [row.Decision, row.Reason].filter(Boolean).join(" · ");
  el("review-note").textContent = row.Note || "";
  const fields = ["Title", "Author"].filter(key => row.Metadata?.[key]);
  show("metadata-unchanged", fields.length === 0);
  show("metadata-diff", fields.length > 0);
  for (const [id, prefix] of [["current-metadata","Old"],["proposed-metadata",""]]) {
    el(id).textContent = fields.map(key => key + ": " + (row.Metadata[prefix+key] || "(blank)")).join("\n");
  }
  const from = parentFolder(row.From), to = row.To ? parentFolder(row.To) : from;
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

function renderLocationTree(from, to) {
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
  const tree = el("location-tree");
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
  const ext = leaf(row.From).split(".").at(-1).toLowerCase();
  try {
    const query = new URLSearchParams({job:latest.Job, index:row.Index, page});
    const response = await fetch("/api/file-preview?" + query, {signal:request.signal,cache:"no-store"});
    if (!response.ok) throw new Error(response.status === 404 && page ? "No more pages" : "Preview unavailable for this file.");
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
    pageOffset = 0;
    for (const other of document.querySelectorAll("[data-review-filter]")) other.setAttribute("aria-pressed", String(other === button));
    updateReview(latest);
  };
}
const activeReviewRow = () => reviewRows.find(row => row.Index === reviewIndex);
function resetRecommendations(key) {
  if (key === recommendationKey) return;
  recommendationRequest?.abort();
  recommendationRequest = null;
  recommendationKey = key;
  chosenRecommendation = "";
  el("recommend-names").disabled = false;
  el("use-recommended-name").disabled = false;
  el("recommended-names-status").textContent = "";
  el("recommended-names-list").replaceChildren();
  show("save-recommended-name",false);
}

el("recommend-names").onclick = async () => {
  const row = activeReviewRow();
  const key = row && [latest.Job,latest.Revision,row.Index].join(":");
  if (!recommendationKey || key !== recommendationKey || recommendationRequest) return;
  const request = new AbortController();
  recommendationRequest = request;
  el("recommend-names").disabled = true;
  el("recommended-names-status").textContent = "Finding names from this file…";
  const query = new URLSearchParams({job:latest.Job,index:row.Index,revision:latest.Revision});
  try {
    const response = await fetch("/api/recommended-names?" + query, {signal:request.signal,cache:"no-store"});
    if (!response.ok) throw new Error((await response.text()).trim());
    const names = await response.json();
    if (request !== recommendationRequest) return;
    chosenRecommendation = "";
    show("save-recommended-name",false);
    el("proposed-filename").textContent = row.To ? leaf(row.To) : "Unchanged";
    el("recommended-names-status").textContent = names.length ? "" : "The file did not provide enough evidence for another name.";
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
    if (request === recommendationRequest) el("recommended-names-status").textContent = err.message;
  } finally {
    if (request === recommendationRequest) {
      recommendationRequest = null;
      el("recommend-names").disabled = false;
    }
  }
};

el("use-recommended-name").onclick = async () => {
  const row = activeReviewRow();
  const key = row && [latest.Job,latest.Revision,row.Index].join(":");
  if (!recommendationKey || key !== recommendationKey || !chosenRecommendation) return;
  const job = latest.Job;
  el("use-recommended-name").disabled = true;
  const saved = await action("correct",{id:job,index:row.Index,revision:latest.Revision,filename:chosenRecommendation});
  el("use-recommended-name").disabled = false;
  if (saved && latest.Job === job && currentView === "review" && reviewFilter === "flagged") {
    reviewIndex = row.Index;
    document.querySelector('[data-review-filter="proposed"]').click();
  }
};

el("delete-flagged-file").onclick = async () => {
  const row = activeReviewRow();
  if (!row || el("delete-flagged-file").hidden ||
      !window.confirm("Move “" + leaf(row.From) + "” to the Windows Recycle Bin?")) return;
  if (await action("recycle-file",{id:latest.Job,indices:[row.Index],revision:latest.Revision})) {
    actionMessage = "Moved " + leaf(row.From) + " to the Recycle Bin.";
    notice(actionMessage);
  }
};
el("review-pause").onclick = () => action("pause");
el("review-undo").onclick = () => el("undo").click();
el("history-review").onclick = () => selectView("review");
el("file-details-tab").onclick = () => selectDetailTab(false);
el("location-tab").onclick = () => selectDetailTab(true);
el("file-prev").onclick = () => loadFilePreview(activeReviewRow(), Math.max(0,previewPage-1));
el("file-next").onclick = () => loadFilePreview(activeReviewRow(), previewPage+1);
el("edit-filename").onclick = () => {
  const row = activeReviewRow();
  if (row) openCorrection({Index:row.Index,Path:row.From,Name:leaf(row.To || row.From)},latest.Job,latest.Revision);
};
el("apply-current").onclick = async () => {
  const row = activeReviewRow();
  if (row && window.confirm("Apply the proposed change for " + leaf(row.From) + "?")) {
    if (await action("apply-one", {id:latest.Job,index:row.Index,revision:latest.Revision})) selectedItems.delete(row.Index);
    renderReviewControls(latest);
  }
};
el("keep-location").onclick = () => changeReviewLocation("");
el("choose-location").onclick = async () => {
  const row = activeReviewRow();
  if (!row) return;
  const picked = await action("pick",{root:parentFolder(row.To || row.From)});
  if (picked?.root && activeReviewRow()?.Index === row.Index) changeReviewLocation(picked.root);
};
function changeReviewLocation(root) {
  const row = activeReviewRow();
  if (row) action("change-location",{root,id:latest.Job,index:row.Index,revision:latest.Revision}).then(result => {
    if (result && !root && leaf(row.From) === leaf(row.To)) selectedItems.delete(row.Index);
    if (result) renderReviewControls(latest);
  });
}
