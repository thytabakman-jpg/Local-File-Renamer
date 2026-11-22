"use strict";

let operationKey = "";
let operationJob = "";
let moveFolders = [];
let selectedFolder = "";
let selectedMove;

async function updateOperationReview(state) {
  const moving = state.Operation === "move";
  show("operation-resume-apply", moving && state.Phase === "apply-paused");
  el("operation-title").textContent = operationNames[state.Operation];
  el("operation-root").textContent = state.Root || "";
  show("operation-pause", state.Busy && !state.Pausing && ["indexing", "starting", "files", "folders", "applying"].includes(state.Phase));
  show("operation-undo", moving && !state.Busy && state.UndoLeft > 0);
  if (state.Busy) {
    operationKey = "";
    show("move-results", false);
    show("duplicate-results", false);
    el("operation-status").textContent = state.ApprovedWaiting ? state.ApprovedWaiting + " approved · waiting to apply" : (phases[state.Phase] || state.Phase) + " · " + state.Done + " of " + state.Total + " items";
    return;
  }
  const key = [state.Job, state.Revision, state.Phase].join(":");
  if (operationKey === key) return;
  operationKey = key;
  if (operationJob !== state.Job) {
    operationJob = state.Job;
    selectedFolder = "";
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
      const count = moveFolders.reduce((total, folder) => total + (folder.Proposals?.length || 0), 0);
      el("operation-status").textContent = summary + count + " proposed moves" + (state.PreviewOnly ? " · Preview only" : "");
    } else {
      drawDuplicateGroups("exact-duplicates", data.Exact, "No exact duplicates found.");
      drawDuplicateGroups("possible-duplicates", data.Possible, "No possible duplicates found.");
      el("operation-status").textContent = summary + (state.Done - data.Skipped) + " files checked · " + data.Skipped + " skipped";
    }
    show("move-results", moving);
    show("duplicate-results", !moving);
  } catch (error) {
    if (operationKey !== key) return;
    show("move-results", false);
    show("duplicate-results", false);
    el("operation-status").textContent = error.message;
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
    if (proposal) renderLocationTree(parentFolder(proposal.From), parentFolder(proposal.To), "move-location-tree");
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

function drawDuplicateGroups(id, groups, empty) {
  const target = el(id);
  if (!groups?.length) {
    target.textContent = empty;
    return;
  }
  target.replaceChildren(...groups.map(group => {
    const list = document.createElement("ul");
    list.className = "duplicate-group";
    list.append(...group.Files.map(path => {
      const file = document.createElement("li");
      file.textContent = path;
      return file;
    }));
    return list;
  }));
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
