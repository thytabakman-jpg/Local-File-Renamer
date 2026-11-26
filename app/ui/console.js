"use strict";

// Section: Console navigation state.

let historyConsoleKey = "";
let settingsCategory = "scope";

// Section: Settings category navigation.

function displaySettingsCategory(category = settingsCategory) {
  settingsCategory = category;
  const groups = {
    scope: ["scope"], naming: ["naming"], execution: ["execution"],
    ai: ["ai", "evidence", "resources", "recovery", "cache"], connector: []
  };
  for (const node of document.querySelectorAll("[data-setting-group]")) {
    node.hidden = !groups[settingsCategory].includes(node.dataset.settingGroup);
    if (!node.hidden && (node.dataset.settingGroup === category || category === "ai" && node.dataset.settingGroup === "ai")) node.open = true;
  }
  for (const button of document.querySelectorAll("[data-preference]")) {
    button.setAttribute("aria-pressed", String(button.dataset.preference === settingsCategory));
  }
  show("next-scan-settings", settingsCategory === "scope");
  show("prototype-connector-fields", settingsCategory === "connector");
  show("prototype-rule-examples", settingsCategory === "naming");
  show("prototype-save-profile", settingsCategory !== "connector");
}

// Section: Scan progress and run history rendering.

function renderConsole(state) {
  for (const radio of document.querySelectorAll('[name="prototype-scan-type"]')) {
    radio.checked = radio.value === el("scan-operation").value;
    radio.disabled = state.Busy;
  }
  const total = state.Total || 0;
  const done = state.Done || 0;
  el("prototype-review-phase").textContent = phases[state.Phase] || state.Phase;
  const progress = el("prototype-rail-review-progress");
  progress.max = total || 1;
  progress.value = done;
  el("prototype-review-progress-text").textContent = total ? `${done} of ${total} items processed` : "Ready to start a scan";
  el("prototype-review-scope").textContent = state.ScopeDescription || "";
  const counts = {all: state.FileTotal, proposed: state.Proposed, auto: state.AutoRenamed, flagged: state.Flagged, kept: state.Kept, unassessed: Math.max(0, (state.FileTotal || 0) - Math.min(done, state.FileTotal || 0)) + (state.FilesNotAssessed || 0)};
  for (const node of document.querySelectorAll("[data-count]")) node.textContent = counts[node.dataset.count] ?? "";

  const historyKey = JSON.stringify([state.Job, state.Jobs, state.Busy]);
  if (historyKey !== historyConsoleKey) {
    historyConsoleKey = historyKey;
    const jobs = state.Jobs || [];
    if (!jobs.length) el("prototype-run-list").textContent = "No saved runs yet";
    else el("prototype-run-list").replaceChildren(...jobs.map(job => {
      const button = document.createElement("button");
      button.type = "button";
      button.disabled = state.Busy;
      button.setAttribute("aria-pressed", String(job.ID === state.Job));
      const name = document.createElement("strong");
      name.textContent = leaf(job.Root) || job.Root;
      const detail = document.createElement("small");
      detail.textContent = [job.Date, operationNames[job.Operation] || "File Renamer"].join(" · ");
      button.append(name, detail);
      button.onclick = () => {
        if (job.ID !== latest.Job) action("select", {id:job.ID});
      };
      return button;
    }));
  }
  el("prototype-run-title").textContent = leaf(state.Root) || "Selected run";
  el("prototype-run-context").textContent = [operationNames[state.Operation], state.Root].filter(Boolean).join(" · ");
  el("prototype-history-assessed").textContent = state.FilesAssessed || 0;
  el("prototype-history-auto").textContent = state.AutoRenamed || 0;
  el("prototype-history-manual").textContent = state.ManualChanges || 0;
  el("prototype-history-flagged").textContent = state.Flagged || 0;
  el("history-review").disabled = !state.Job;
  el("prototype-chat-run").textContent = [operationNames[state.Operation], phases[state.Phase]].filter(Boolean).join(" · ");
}

for (const radio of document.querySelectorAll('[name="prototype-scan-type"]')) {
  radio.onchange = () => {
    el("scan-operation").value = radio.value;
    el("scan-operation").dispatchEvent(new Event("change"));
  };
}
for (const button of document.querySelectorAll("[data-preference]")) {
  button.onclick = () => displaySettingsCategory(button.dataset.preference);
}
for (const button of document.querySelectorAll("[data-chat-example]")) {
  button.onclick = () => {
    el("chat-input").value = button.dataset.chatExample;
    el("chat-form").requestSubmit();
  };
}
displaySettingsCategory();
