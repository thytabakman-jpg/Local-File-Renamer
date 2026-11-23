"use strict";

let operationKey = "";
let operationJob = "";
let moveFolders = [];
let selectedFolder = "";
let selectedMove;
let duplicateView = "exact";
let duplicateSelections = [];
let verifiedGroups = [];
let verifiedCanRecycle = false;
let skippedRecommendations = new Set();
let recycleBatchRunning = false;

el("retry-operation-results").onclick = () => {
  operationKey = "";
  updateOperationReview(latest);
};

async function updateOperationReview(state) {
  const moving = state.Operation === "move";
  show("prototype-move-rail", moving && !state.Busy);
  show("prototype-duplicate-nav", !moving);
  el("prototype-operation-phase").textContent = phases[state.Phase] || state.Phase;
  show("operation-resume-apply", moving && state.Phase === "apply-paused");
  el("operation-title").textContent = operationNames[state.Operation];
  el("operation-root").textContent = state.Root || "";
  show("operation-pause", state.Busy && !state.Pausing && ["indexing", "starting", "files", "folders", "applying"].includes(state.Phase));
  show("operation-undo", moving && !state.Busy && state.UndoLeft > 0);
  if (state.Busy) {
    operationKey = "";
    show("retry-operation-results", false);
    show("move-results", false);
    show("duplicate-results", false);
    el("operation-status").textContent = state.ApprovedWaiting ? state.ApprovedWaiting + " approved · waiting to apply" : (phases[state.Phase] || state.Phase) + " · " + state.Done + " of " + state.Total + " items";
    return;
  }
  const key = [state.Job, state.Revision, state.Phase].join(":");
  if (operationKey === key) return;
  operationKey = key;
  show("retry-operation-results", false);
  show("move-results", false);
  show("duplicate-results", false);
  el("operation-status").textContent = "Loading scan results…";
  if (operationJob !== state.Job) {
    operationJob = state.Job;
    selectedFolder = "";
    skippedRecommendations.clear();
    el("duplicate-recycle-receipt").replaceChildren();
  }
  try {
    const response = await fetch("/api/scan-results?" + new URLSearchParams({ job: state.Job }), {
      cache: "no-store", signal: AbortSignal.timeout(5000)
    });
    if (!response.ok) throw new Error((await response.text()).trim());
    const data = await response.json();
    if (operationKey !== key || latest.Job !== state.Job) return;
    const summary = state.ApprovedWaiting ? state.ApprovedWaiting + " approved · waiting to apply. " : state.Phase === "paused" ? "Partial results from a paused scan. " : "";
    if (moving) {
      moveFolders = data.Folders || [];
      selectedFolder = (moveFolders.find(folder => folder.Path === selectedFolder) ||
        moveFolders.find(folder => folder.Proposals?.length) || moveFolders[0])?.Path || "";
      drawMoveReview();
      if (el("kept-locations").open) loadKeptLocations(state, 0);
      const count = moveFolders.reduce((total, folder) => total + (folder.Proposals?.length || 0), 0);
      el("operation-status").textContent = summary + (count ? count + " proposed moves" : state.OutcomeReason || "No proposed moves remain") + (state.PreviewOnly ? " · Preview only" : "");
    } else {
      duplicateSelections = [];
      drawExactDuplicates(data.Exact, data.CanRecycle, state);
      updateDuplicateApproval();
      verifiedGroups = data.Verified || [];
      verifiedCanRecycle = data.CanRecycle;
      const unresolved = data.Unresolved || 0;
      el("verified-duplicate-summary").textContent = unresolved ?
        unresolved + " candidate groups left alone because equivalence could not be confirmed." : "";
      drawVerifiedDuplicates(state);
      el("operation-status").textContent = summary + (state.Done - data.Skipped) + " files checked · " + data.Skipped + " skipped" + (data.Recycled ? " · " + data.Recycled + " recycled" : "");
    }
    show("move-results", moving);
    show("duplicate-results", !moving);
    if (!moving) selectDuplicateView(duplicateView);
  } catch (error) {
    if (operationKey !== key) return;
    show("move-results", false);
    show("duplicate-results", false);
    el("operation-status").textContent = "Could not load scan results. " + error.message;
    show("retry-operation-results", true);
  }
}

el("kept-locations").ontoggle = () => {
  if (el("kept-locations").open && latest.Operation === "move" && !latest.Busy) loadKeptLocations(latest, 0);
};

async function loadKeptLocations(state, offset) {
  const query = new URLSearchParams({job: state.Job, revision: state.Revision, offset});
  try {
    const response = await fetch("/api/kept-locations?" + query, {cache: "no-store"});
    if (!response.ok) throw new Error((await response.text()).trim());
    const page = await response.json();
    if (latest.Job !== state.Job || latest.Revision !== state.Revision) return;
    el("kept-location-rows").replaceChildren(...page.Rows.map(row => {
      const item = document.createElement("li");
      item.textContent = leaf(row.Path) + " · " + parentFolder(row.Path) + " · " + row.Reason;
      return item;
    }));
    el("kept-location-status").textContent = page.Total ? (offset + 1) + "–" + (offset + page.Rows.length) + " of " + page.Total : "No files were kept in place.";
    const next = el("kept-location-next");
    show("kept-location-next", Boolean(page.NextOffset));
    next.onclick = () => loadKeptLocations(state, page.NextOffset);
  } catch (error) {
    if (latest.Job === state.Job && latest.Revision === state.Revision) el("kept-location-status").textContent = error.message;
  }
}

function drawMoveReview() {
  const byParent = new Map();
  for (const folder of moveFolders) {
    if (!byParent.has(folder.Parent)) byParent.set(folder.Parent, []);
    byParent.get(folder.Parent).push(folder);
  }
  const renderFolder = folder => {
    const li = document.createElement("li");
    const button = document.createElement("button");
    button.type = "button";
    button.className = "quiet";
    button.dataset.folderPath = folder.Path;
    button.setAttribute("aria-pressed", String(folder.Path === selectedFolder));
    button.textContent = leaf(folder.Path) + (folder.New ? " · New" : "") + " (" + (folder.Proposals?.length || 0) + ")";
    button.onclick = () => {
      selectedFolder = folder.Path;
      drawMoveReview();
      [...el("move-tree").querySelectorAll("button")].find(node => node.dataset.folderPath === selectedFolder)?.focus();
    };
    li.append(button);
    const children = byParent.get(folder.Path);
    if (children?.length) {
      const list = document.createElement("ul");
      list.append(...children.map(renderFolder));
      li.append(list);
    }
    return li;
  };
  el("move-tree").replaceChildren(...(byParent.get("") || []).map(renderFolder));
  const folder = moveFolders.find(item => item.Path === selectedFolder);
  el("move-destination").textContent = leaf(folder?.Path);
  el("move-folder-note").textContent = folder?.New ? "Create this folder when applying the selected moves." : (folder?.Path || "");
  const selectMove = proposal => {
    selectedMove = proposal;
    show("move-location-actions", Boolean(proposal && canReview(latest) && !latest.PreviewOnly));
    for (const button of el("move-files").querySelectorAll("button[data-move-index]")) {
      button.setAttribute("aria-pressed", String(Number(button.dataset.moveIndex) === proposal?.Index));
    }
    if (proposal) {
      el("move-from").textContent = parentFolder(proposal.From);
      el("move-to").textContent = parentFolder(proposal.To);
      renderLocationTree(parentFolder(proposal.From), parentFolder(proposal.To), "move-location-tree");
    }
    else el("move-location-tree").replaceChildren();
  };
  const rows = (folder?.Proposals || []).map(proposal => {
    const row = document.createElement("li");
    const label = document.createElement("label");
    const checkbox = document.createElement("input");
    const description = document.createElement("span");
    const title = document.createElement("button");
    const details = document.createElement("small");
    checkbox.type = "checkbox";
    checkbox.value = String(proposal.Index);
    checkbox.disabled = latest.PreviewOnly || !canReview(latest);
    checkbox.onchange = () => {
      const count = el("move-files").querySelectorAll("input:checked").length;
      el("apply-moves").disabled = count === 0;
      el("apply-moves").textContent = count ? "Apply selected moves (" + count + ")" : "Apply selected moves";
    };
    title.type = "button";
    title.className = "quiet";
    title.dataset.moveIndex = proposal.Index;
    title.textContent = leaf(proposal.From);
    title.onclick = () => selectMove(proposal);
    checkbox.onfocus = () => selectMove(proposal);
    details.textContent = [parentFolder(proposal.From), proposal.Reason, proposal.Note].filter(Boolean).join(" · ");
    description.append(title, details);
    checkbox.setAttribute("aria-label", "Select move for " + leaf(proposal.From));
    label.append(checkbox);
    row.append(label, description);
    return row;
  });
  if (!rows.length) {
    const row = document.createElement("li");
    row.textContent = "No moves proposed for this folder.";
    rows.push(row);
  }
  el("move-files").replaceChildren(...rows);
  selectMove(folder?.Proposals?.find(p => p.Index === selectedMove?.Index) || folder?.Proposals?.[0]);
  el("apply-moves").disabled = true;
  el("apply-moves").textContent = "Apply selected moves";
}

function selectDuplicateView(view) {
  duplicateView = view;
  for (const button of document.querySelectorAll("[data-duplicate-view]")) {
    button.setAttribute("aria-pressed", String(button.dataset.duplicateView === view));
  }
  for (const panel of document.querySelectorAll("[data-duplicate-panel]")) {
    panel.hidden = panel.dataset.duplicatePanel !== view;
  }
}

for (const button of document.querySelectorAll("[data-duplicate-view]")) {
  button.onclick = () => selectDuplicateView(button.dataset.duplicateView);
}

function updateDuplicateApproval() {
  const groups = duplicateSelections.map(get => get()).filter(group => group.indices.length);
  const button = el("accept-duplicate-recommendations");
  show("accept-duplicate-recommendations", duplicateSelections.length > 0);
  button.disabled = recycleBatchRunning || !groups.length;
  button.textContent = groups.length ? "Accept recommendations (" + groups.length + " groups)" : "Accept recommendations";
}

el("accept-duplicate-recommendations").onclick = async () => {
  if (recycleBatchRunning) return;
  const groups = duplicateSelections.map(get => get()).filter(group => group.indices.length);
  if (!groups.length) return;
  const count = groups.reduce((sum, group) => sum + group.indices.length, 0);
  if (!window.confirm("Keep the selected originals and send " + count + " exact copies from " + groups.length + " groups to the Recycle Bin?")) return;
  submitDuplicateApproval(latest, groups, "recycle-duplicate-groups");
};

function approvalGroups() {
  return verifiedGroups.filter(group => !skippedRecommendations.has(group.RecommendedKeeper));
}

function updateVerifiedApproval() {
  const button = el("approve-all-verified");
  show("approve-all-verified", verifiedCanRecycle && approvalGroups().length > 0);
  button.disabled = recycleBatchRunning;
  for (const approve of el("verified-duplicates").querySelectorAll("[data-approve-copy]")) {
    approve.disabled = recycleBatchRunning;
  }
}

function drawVerifiedDuplicates(state) {
  const target = el("verified-duplicates");
  const groups = approvalGroups();
  target.replaceChildren(...groups.map(group => {
    const panel = document.createElement("fieldset");
    panel.className = "operation-duplicate-group";
    const keeper = group.Members.find(member => member.Index === group.RecommendedKeeper);
    const copies = group.Members.filter(member => member.Index !== keeper.Index);
    const finding = document.createElement("p");
    finding.textContent = "What we found: " + group.Finding;
    const recommendation = document.createElement("p");
    recommendation.textContent = "Recommendation: Keep " + keeper.Path + ". " + group.KeeperReason;
    const summary = document.createElement("summary");
    summary.textContent = copies.length + (copies.length === 1 ? " copy" : " copies") + " to recycle";
    const details = document.createElement("details");
    const paths = document.createElement("ul");
    for (const copy of copies) {
      const row = document.createElement("li");
      row.textContent = copy.Path;
      paths.append(row);
    }
    details.append(summary, paths);
    details.open = true;
    panel.append(finding, recommendation, details);
    const actions = document.createElement("div");
    actions.className = "actions";
    const compare = document.createElement("button");
    compare.type = "button";
    compare.className = "secondary";
    compare.textContent = "View comparison";
    const previews = document.createElement("div");
    previews.className = "duplicate-comparison";
    compare.setAttribute("aria-expanded", "false");
    compare.onclick = () => {
      const open = compare.getAttribute("aria-expanded") === "true";
      compare.setAttribute("aria-expanded", String(!open));
      compare.textContent = open ? "View comparison" : "Hide comparison";
      previews.replaceChildren();
      if (!open) for (const member of [keeper, ...copies]) renderComparisonPreview(previews, state.Job, member);
    };
    if (verifiedCanRecycle) {
      const approve = document.createElement("button");
      approve.type = "button";
      approve.className = "primary";
      approve.dataset.approveCopy = "";
      approve.textContent = "Approve";
      approve.onclick = () => confirmVerifiedDuplicates(state, [group]);
      actions.append(approve);
    }
    const skip = document.createElement("button");
    skip.type = "button";
    skip.className = "secondary";
    skip.textContent = "Skip";
    skip.onclick = () => {
      skippedRecommendations.add(group.RecommendedKeeper);
      drawVerifiedDuplicates(state);
    };
    actions.append(skip, compare);
    panel.append(actions, previews);
    return panel;
  }));
  if (!groups.length) {
    target.textContent = skippedRecommendations.size ? "No remaining cleanup recommendations. Skipped files were kept." :
      "No verified cleanup recommendations.";
  }
  if (!verifiedCanRecycle && groups.length) {
    const note = document.createElement("p");
    note.textContent = state.PreviewOnly ? "Preview only. Start a new scan with Review enabled to recycle copies." : "The Recycle Bin is available on Windows.";
    target.prepend(note);
  }
  updateVerifiedApproval();
}

el("approve-all-verified").onclick = () => confirmVerifiedDuplicates(latest, approvalGroups());

function confirmVerifiedDuplicates(state, groups) {
  if (recycleBatchRunning || !verifiedCanRecycle || !groups.length) return;
  const dialog = el("duplicate-approval-dialog");
  const files = el("duplicate-approval-files");
  files.replaceChildren();
  const selections = groups.map(group => {
    const keep = group.RecommendedKeeper;
    const copies = group.Members.filter(member => member.Index !== keep);
    const label = document.createElement("p");
    label.textContent = "Keep " + group.Members.find(member => member.Index === keep).Path;
    const list = document.createElement("ul");
    for (const copy of copies) {
      const item = document.createElement("li");
      item.textContent = "Recycle " + copy.Path;
      list.append(item);
    }
    files.append(label, list);
    return {keep, indices: copies.map(copy => copy.Index)};
  });
  dialog.returnValue = "cancel";
  dialog.addEventListener("close", () => {
    if (dialog.returnValue === "approve") submitDuplicateApproval(state, selections, "recycle-verified-duplicates");
  }, {once: true});
  dialog.showModal();
}

async function submitDuplicateApproval(state, groups, actionName) {
  const {Job, Revision} = state;
  if (recycleBatchRunning || latest.Job !== Job || latest.Revision !== Revision) return;
  const count = groups.reduce((sum, group) => sum + group.indices.length, 0);
  recycleBatchRunning = true;
  el("duplicate-recycle-receipt").textContent = "Checking selected copies before recycling…";
  updateDuplicateApproval();
  updateVerifiedApproval();
  try {
    const result = await action(actionName, {id:Job, revision:Revision, duplicateGroups:groups});
    if (latest.Job !== Job) return;
    if (!result) {
      el("duplicate-recycle-receipt").textContent = "The recycle request failed. Check the scan results before retrying.";
      return;
    }
    const receipt = el("duplicate-recycle-receipt");
    const text = document.createElement("p");
    text.textContent = result.Recycled + " of " + count + " copies recycled." + (result.Error ? " " + result.Error : "");
    const list = document.createElement("ul");
    for (const group of result.Groups || []) {
      const item = document.createElement("li");
      item.textContent = group.Kept + " · " + group.Recycled + " of " + group.Requested + " copies recycled";
      list.append(item);
    }
    receipt.replaceChildren(text, list);
  } finally {
    recycleBatchRunning = false;
    updateDuplicateApproval();
    updateVerifiedApproval();
  }
}

function drawExactDuplicates(groups, canRecycle, state) {
  const id = "exact-duplicates";
  const target = el(id);
  if (!groups?.length) {
    target.textContent = "No exact duplicates found.";
    return;
  }
  target.replaceChildren(...groups.map((group, order) => {
    const panel = document.createElement("fieldset");
    panel.className = "operation-duplicate-group";
    const legend = document.createElement("legend");
    legend.textContent = group.Grouping + " · " + group.Members.length + " files";
    panel.append(legend);
    const rows = [];
    const selections = [];
    let keep = group.RecommendedKeeper;
    for (const member of group.Members) {
      const line = document.createElement("div");
      line.className = "duplicate-file-row";
      const details = document.createElement("span");
      details.textContent = member.Path;
      if (canRecycle) {
        const controls = document.createElement("span");
        const radio = document.createElement("input");
        radio.name = id + "-keep-" + order;
        radio.type = "radio";
        radio.checked = member.Index === keep;
        radio.setAttribute("aria-label", "Keep " + member.Path);
        const recycle = document.createElement("input");
        recycle.type = "checkbox";
        recycle.disabled = radio.checked;
        recycle.checked = !radio.checked;
        recycle.setAttribute("aria-label", "Recycle " + member.Path);
        radio.onchange = () => {
          const previous = keep;
          keep = member.Index;
          for (const item of selections) {
            item.box.disabled = item.index === keep;
            if (item.index === keep) item.box.checked = false;
            if (item.index === previous) item.box.checked = true;
          }
          updateDuplicateApproval();
        };
        recycle.onchange = updateDuplicateApproval;
        controls.append(radio,recycle);
        line.append(controls);
        selections.push({index:member.Index,box:recycle});
      }
      line.append(details);
      rows.push(line);
    }
    if (canRecycle) {
      const labels = document.createElement("p");
      labels.className = "run-hint";
      labels.textContent = group.KeeperReason + " Change the keeper or uncheck any copy you want to keep.";
      duplicateSelections.push(() => ({keep, indices:selections.filter(item => item.box.checked).map(item => item.index)}));
      panel.append(labels);
    }
    panel.append(...rows);
    const actions = document.createElement("div");
    actions.className = "actions";
    if (!canRecycle) {
      const disabled = document.createElement("p");
      disabled.className = "run-hint";
      disabled.textContent = state.PreviewOnly ? "Preview only. Start a new scan with Review enabled to recycle copies." : "The Recycle Bin is available on Windows.";
      panel.append(disabled);
    }
    const picks = [0,1].map((start,side) => {
      const select = document.createElement("select");
      select.setAttribute("aria-label", side ? "Second file to compare" : "First file to compare");
      select.replaceChildren(...group.Members.map(member => new Option(member.Path,String(member.Index))));
      select.selectedIndex = start;
      return select;
    });
    const button = document.createElement("button");
    button.type = "button";
    button.className = "secondary";
    button.textContent = "Compare files";
    const status = document.createElement("p");
    status.setAttribute("role","status");
    const preview = document.createElement("div");
    preview.className = "duplicate-comparison";
    let compared = "";
    let report;
    let request;
    let requestKey;
    let requestController;
    async function compare() {
      const [first, second] = picks.map(pick => pick.value);
      if (first === second) {
        status.textContent = "Choose two different files.";
        return;
      }
      const key = first + ":" + second;
      if (key === compared) return;
      if (request && requestKey === key && !requestController.signal.aborted) return request;
      requestController?.abort();
      const controller = new AbortController();
      requestController = controller;
      requestKey = key;
      request = (async () => {
        status.textContent = "Checking the selected pair…";
        preview.replaceChildren();
        try {
          const query = new URLSearchParams({job:state.Job,first,second});
          const signals = [controller.signal, AbortSignal.timeout(30000)];
          const response = await fetch("/api/compare-duplicates?" + query, {signal:AbortSignal.any(signals)});
          if (!response.ok) throw new Error((await response.text()).trim());
          const result = await response.json();
          if (controller.signal.aborted || !panel.isConnected || picks.map(pick => pick.value).join(":") !== key) return;
          compared = key;
          report = result;
          status.textContent = result.Recommendation + " · " + result.Verdict + " · " + result.Detail;
        } catch (err) {
          if (!controller.signal.aborted && picks.map(pick => pick.value).join(":") === key) {
            status.textContent = "Needs your judgment. " + err.message;
          }
        } finally {
          if (requestController === controller) request = undefined;
        }
      })();
      return request;
    }
    for (const pick of picks) {
      pick.onchange = () => {
        requestController?.abort();
        compared = "";
        report = undefined;
        preview.replaceChildren();
        status.textContent = "";
      };
    }
    button.onclick = async () => {
      button.disabled = true;
      try {
        await compare();
        const values = picks.map(pick => pick.value);
        if (report && compared === values.join(":")) {
          preview.replaceChildren();
          for (const value of values) renderComparisonPreview(preview, state.Job, group.Members.find(member => String(member.Index) === value));
        }
      } finally {
        button.disabled = false;
      }
    };
    actions.append(...picks,button);
    panel.append(actions,status,preview);
    return panel;
  }));
}

function renderComparisonPreview(previews, job, file) {
  const panel = document.createElement("section");
  const title = document.createElement("h3");
  title.textContent = leaf(file.Path);
  const content = document.createElement("div");
  const pagination = document.createElement("div");
  pagination.className = "actions";
  panel.append(title,content,pagination);
  previews.append(panel);
  const pdf = file.Path.toLowerCase().endsWith(".pdf");
  let page = 0;
  const nav = delta => {
    const button = document.createElement("button");
    button.type = "button";
    button.className = "secondary";
    button.textContent = delta < 0 ? "Previous page" : "Next page";
    button.onclick = () => load(page + delta);
    return button;
  };
  const previous = nav(-1), next = nav(1), pageNumber = document.createElement("span");
  if (pdf) pagination.append(previous,pageNumber,next);
  async function load(requested) {
    const query = new URLSearchParams({job,index:file.Index,page:requested});
    if (!content.hasChildNodes()) content.textContent = "Loading preview…";
    previous.disabled = next.disabled = true;
    try {
      const response = await fetch("/api/file-preview?" + query, {signal:AbortSignal.timeout(40000)});
      if (!response.ok) throw new Error((await response.text()).trim());
      if (response.headers.get("Content-Type")?.includes("image/jpeg")) {
        const url = URL.createObjectURL(await response.blob());
        const image = document.createElement("img");
        image.onload = image.onerror = () => URL.revokeObjectURL(url);
        image.alt = pdf ? title.textContent + " · Page " + (requested + 1) : title.textContent;
        image.src = url;
        content.replaceChildren(image);
      } else {
        const result = await response.json();
        const paper = document.createElement("div");
        paper.className = "preview-paper";
        paper.replaceChildren(...result.Paragraphs.map(paragraph => {
          const line = document.createElement("p");
          line.textContent = paragraph.Text;
          return line;
        }));
        content.replaceChildren(paper);
      }
      page = requested;
      next.disabled = false;
    } catch (err) {
      if (requested <= page || !content.querySelector("img")) content.textContent = err.message || "Preview unavailable.";
    } finally {
      pageNumber.textContent = "Page " + (page + 1);
      previous.disabled = page === 0;
    }
  }
  load(0);
}

el("apply-moves").onclick = () => {
  const folder = moveFolders.find(item => item.Path === selectedFolder);
  const indices = [...el("move-files").querySelectorAll("input:checked")].map(input => Number(input.value));
  if (!folder || !indices.length) return;
  const move = "Move " + indices.length + " selected files to " + folder.Path + "?";
  const confirmation = folder.New ? "Create the destination folder and move " + indices.length + " selected files to " + folder.Path + "?" : move;
  if (window.confirm(confirmation)) {
    action("apply-selected", { id: latest.Job, revision: latest.Revision, indices,
      rowRevisions: Object.fromEntries(folder.Proposals.filter(p => indices.includes(p.Index)).map(p => [p.Index,p.Revision])) });
  }
};
el("operation-pause").onclick = () => action("pause");
el("operation-undo").onclick = () => {
  if (window.confirm("Undo applied moves for this scan?")) action("undo");
};

el("operation-resume-apply").onclick = () => action("apply", {id:latest.Job, revision:latest.Revision});
el("move-keep-location").onclick = () => setMoveLocation("");
el("move-change-location").onclick = async () => {
  const move = selectedMove;
  if (!move) return;
  const picked = await action("pick", {root:parentFolder(move.To)});
  if (picked?.root && selectedMove?.Index === move.Index) setMoveLocation(picked.root);
};
async function setMoveLocation(root) {
  if (!selectedMove) return;
  await action("change-location", {id:latest.Job, revision:selectedMove.Revision, index:selectedMove.Index, root});
}
