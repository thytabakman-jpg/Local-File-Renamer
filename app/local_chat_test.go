package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalChatPreviewApplyAndUndo(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "picked")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(root, "MODEL_GUIDE.txt")
	second := filepath.Join(root, "SECOND_FILE.txt")
	writeTestFile(t, first, "Model Guide\nA prompt about the local assistant.")
	writeTestFile(t, second, "Second File\nA file to retain.")
	app, err := newApp(filepath.Join(base, "profile"), filepath.Join(base, "engine"), filepath.Join(base, "models"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app.chat.cancel(); app.s.close() })
	var queries int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var payload struct{ Messages []struct{ Content string } }
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
			return
		}
		var user map[string]string
		if err := json.Unmarshal([]byte(payload.Messages[len(payload.Messages)-1].Content), &user); err != nil {
			t.Error(err)
			return
		}
		queries++
		var plan chatPlan
		switch user["request"] {
		case "Hello. How can you help?":
			plan = chatPlan{Action: "reply", Message: "I can preview filenames."}
		case "Please preview the selected folder using the naming rules only.":
			plan = chatPlan{Action: "start", WithoutAI: true, Include: []string{".txt"}}
		case "Apply only proposal 0.":
			if !strings.Contains(user["recent_conversation"], "0: MODEL_GUIDE.txt") {
				t.Error("proposal was absent from chat history")
			}
			plan = chatPlan{Action: "apply", Indices: []int{0}}
		case "Undo the rename I just made.":
			plan = chatPlan{Action: "undo"}
		default:
			t.Error("unexpected request", user)
		}
		data, _ := json.Marshal(plan)
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": string(data)}}}})
	}))
	defer backend.Close()
	opts := defaultSettings()
	app.chat.model = &localModel{url: backend.URL, settings: &opts}
	handler := server(app).Handler
	home := httptest.NewRecorder()
	handler.ServeHTTP(home, httptest.NewRequest("GET", "http://127.0.0.1:39173/", nil))
	cookie := home.Result().Cookies()[0]
	send := func(input map[string]string, authenticated bool) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(input)
		req := httptest.NewRequest("POST", "http://127.0.0.1:39173/api/chat", bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		if authenticated {
			req.AddCookie(cookie)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	if result := send(map[string]string{"Message": "status"}, false); result.Code != 403 {
		t.Fatal(result.Code)
	}
	chat := func(message string) chatReply {
		t.Helper()
		result := send(map[string]string{"Message": message, "Folder": root}, true)
		if result.Code != 200 {
			t.Fatalf("%d: %s", result.Code, result.Body)
		}
		var reply chatReply
		if err := json.Unmarshal(result.Body.Bytes(), &reply); err != nil {
			t.Fatal(err)
		}
		return reply
	}
	if reply := chat("Hello. How can you help?"); reply.Message != "I can preview filenames." {
		t.Fatal(reply)
	}
	chat("Please preview the selected folder using the naming rules only.")
	waitForPhase(t, app, "preview")
	if reply := chat("show proposals"); len(reply.Proposals) != 2 {
		t.Fatal(reply)
	}
	apply := chat("Apply only proposal 0.")
	if apply.Approval == nil || len(apply.Proposals) != 1 || filepath.Base(apply.Proposals[0].From) != filepath.Base(first) {
		t.Fatal(apply)
	}
	if _, err := os.Stat(first); err != nil {
		t.Fatal("chat altered a file before approval", err)
	}
	if result := send(map[string]string{"Approve": "invented"}, true); result.Code != 409 {
		t.Fatal(result.Code)
	}
	if result := send(map[string]string{"Approve": apply.Approval.Token}, true); result.Code != 200 {
		t.Fatalf("%d: %s", result.Code, result.Body)
	}
	waitForPhase(t, app, "preview")
	if v := app.snapshot(); v.Proposed != 1 {
		t.Fatalf("unchecked proposal was not retained: %+v", v)
	}
	if _, err := os.Stat(filepath.Join(root, "Model Guide.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(second); err != nil {
		t.Fatal(err)
	}
	if result := send(map[string]string{"Approve": apply.Approval.Token}, true); result.Code != 409 {
		t.Fatal("reused approval accepted")
	}
	undo := chat("Undo the rename I just made.")
	if undo.Approval == nil {
		t.Fatal(undo)
	}
	if result := send(map[string]string{"Approve": undo.Approval.Token}, true); result.Code != 200 {
		t.Fatalf("%d: %s", result.Code, result.Body)
	}
	waitForPhase(t, app, "undone")
	if _, err := os.Stat(first); err != nil {
		t.Fatal(err)
	}
	if queries != 4 {
		t.Fatal(fmt.Sprintf("unexpected model request count: %d", queries))
	}
}
