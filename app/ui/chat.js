"use strict";

const chatEl = id => document.getElementById(id);
let localChatBusy = false;
let chatScanVersion = 0;

function addChatMessage(role, text) {
  const article = document.createElement("article");
  article.className = "chat-message";
  const name = document.createElement("strong");
  name.textContent = role;
  const body = document.createElement("p");
  body.textContent = text;
  article.append(name, body);
  chatEl("chat-log").append(article);
  chatEl("chat-log").scrollTop = chatEl("chat-log").scrollHeight;
  return article;
}

function showChatReply(reply) {
  const message = addChatMessage("Renamer", reply.Message);
  for (const group of reply.Groups || []) {
    const label = document.createElement("strong");
    label.textContent = group.Label;
    const list = document.createElement("ul");
    for (const path of group.Files) {
      const item = document.createElement("li");
      item.textContent = path;
      list.append(item);
    }
    message.append(label, list);
  }
  if (reply.Proposals?.length) {
    const list = document.createElement("ul");
    for (const row of reply.Proposals) {
      const item = document.createElement("li");
      const renaming = row.To ? row.From + " → " + row.To : row.From;
      const metadata = [row.Metadata?.Title && "Title: " + row.Metadata.Title, row.Metadata?.Author && "Author: " + row.Metadata.Author].filter(Boolean);
      item.textContent = [row.Index + ": " + renaming, row.Draft && "Draft", ...metadata, row.Reason, row.Note].filter(Boolean).join(" · ");
      list.append(item);
    }
    message.append(list);
  }
  if (reply.Sources?.length) {
    const list = document.createElement("ul");
    for (const source of reply.Sources) {
      const item = document.createElement("li");
      const link = document.createElement("a");
      if (!source.URL.startsWith("https://")) continue;
      link.href = source.URL;
      link.target = "_blank";
      link.rel = "noopener noreferrer";
      link.textContent = source.Title;
      item.append(link);
      if (source.Detail) item.append(document.createTextNode(" · " + source.Detail));
      list.append(item);
    }
    message.append(list);
  }
  if (reply.Approval) {
    const button = document.createElement("button");
    button.type = "button";
    button.className = "secondary";
    button.textContent = reply.Approval.Label;
    button.addEventListener("click", async () => {
      button.disabled = true;
      if (!await sendChat({Approve:reply.Approval.Token})) button.disabled = false;
    });
    message.append(button);
  }
  chatEl("chat-log").scrollTop = chatEl("chat-log").scrollHeight;
  return message;
}

async function watchChatScan(reply) {
  const version = ++chatScanVersion;
  const query = new URLSearchParams({ id:reply.ScanID, exact:reply.ExactOnly ? "1" : "0", results:reply.ShowResults ? "1" : "0" });
  try {
    while (!closed && !closing && version === chatScanVersion) {
      await new Promise(resolve => setTimeout(resolve, 1000));
      if (closed || closing || version !== chatScanVersion) return;
      const response = await fetch("/api/chat-scan?" + query, {cache:"no-store", signal:AbortSignal.timeout(5000)});
      if (closed || closing || version !== chatScanVersion) return;
      if (!response.ok) throw new Error((await response.text()).trim());
      const result = await response.json();
      if (closed || closing || version !== chatScanVersion) return;
      if (!result.ScanID) {
        showChatReply(result);
        return;
      }
    }
  } catch (error) {
    if (!closed && !closing && version === chatScanVersion) addChatMessage("Renamer", error.message);
  }
}

async function sendChat(request) {
  if (localChatBusy) return false;
  localChatBusy = true;
  chatEl("chat-send").disabled = chatEl("reset-chat").disabled = true;
  chatEl("chat-status").textContent = request.Message ? "Waiting for a local reply…" : "";
  try {
    const response = await fetch("/api/chat", {
      method:"POST", headers:{"Content-Type":"application/json"}, body:JSON.stringify(request)
    });
    if (!response.ok) throw new Error((await response.text()).trim());
    const reply = await response.json();
    if (request.Reset) {
      ++chatScanVersion;
      chatEl("chat-log").replaceChildren();
    } else {
      if (reply.Folder) {
        folderEdited = true;
        chatEl("folder").value = reply.Folder;
        chatEl("chat-folder").textContent = "Selected scan folder: " + reply.Folder;
      }
      if (reply.SettingsChanged) await loadSettings("", true);
      const message = showChatReply(reply);
      if (reply.OpenReview) {
        await refresh();
        const review = document.createElement("button");
        review.textContent = "Show review";
        review.className = "secondary";
        review.onclick = () => selectView("review");
        message.append(review);
      }
      if (reply.ScanID) watchChatScan(reply);
    }
    return true;
  } catch (error) {
    addChatMessage("Renamer", error.message);
    return false;
  } finally {
    localChatBusy = false;
    chatEl("chat-status").textContent = "";
    chatEl("chat-send").disabled = chatEl("reset-chat").disabled = false;
  }
}

chatEl("reset-chat").addEventListener("click", () => sendChat({Reset:true}));
chatEl("chat-form").addEventListener("submit", event => {
  event.preventDefault();
  if (localChatBusy) return;
  const input = chatEl("chat-input");
  const message = input.value.trim();
  if (!message) return;
  addChatMessage("You", message);
  input.value = "";
  sendChat({Message:message, Folder:chatEl("folder").value.trim(), Model:chatEl("chat-model").value, Operation:chatEl("scan-operation").value});
});
