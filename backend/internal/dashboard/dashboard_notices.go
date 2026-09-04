package dashboard

import (
	"fmt"
	"time"

	"freebuff-proxy/backend/internal/upstream"
)

// NoticeItem represents one surfaced upstream announcement, peak window,
// or live broadcast.
type NoticeItem struct {
	ID        string `json:"id"`
	Type      string `json:"type"` // "announcement" | "peak_hours" | "upgrade_hint" | "server_message"
	Title     string `json:"title"`
	Message   string `json:"message"`
	URL       string `json:"url,omitempty"`
	Badge     string `json:"badge,omitempty"`
	Tone      string `json:"tone"` // "info" | "warning" | "accent"
	TokenIdx  *int   `json:"token_index,omitempty"`
	Timestamp string `json:"timestamp,omitempty"`
}

// NoticesResponse is the payload returned by GET /admin/api/notices.
type NoticesResponse struct {
	Notices   []NoticeItem                     `json:"notices"`
	PeakHours upstream.DeepSeekPeakHoursWindow `json:"peak_hours"`
	Count     int                              `json:"count"`
}

// noticesData aggregates upstream static announcements, live DeepSeek peak
// hours, and dynamic per-token server messages/hints for the dashboard.
func (d *Dashboard) noticesData() NoticesResponse {
	var list []NoticeItem

	// 1. Official Upstream Tier & Model Announcement
	list = append(list, NoticeItem{
		ID:      "upstream-tier-change",
		Type:    "announcement",
		Title:   "Official Upstream Announcement",
		Message: upstream.TierChangeNotice,
		Badge:   "Freebuff Team",
		Tone:    "accent",
	})

	// 2. Live DeepSeek Peak Hours Evaluation
	peak := upstream.EvaluateDeepSeekPeak(time.Now())
	if peak.IsPeak {
		list = append(list, NoticeItem{
			ID:      "deepseek-peak-active",
			Type:    "peak_hours",
			Title:   "DeepSeek Peak Hours Active",
			Message: fmt.Sprintf("DeepSeek models are currently in peak pricing window (%s - %s). Normal pricing resumes in %s.", peak.WindowStartUTC, peak.WindowEndUTC, peak.NextWindowIn),
			Badge:   "Peak Window",
			Tone:    "warning",
		})
	}

	// 3. Dynamic Session Broadcasts & Upgrade Hints from Pool Tokens
	if d.pool != nil {
		snaps := d.pool.Snapshot()
		for i, tok := range snaps {
			idx := i
			if tok.UpgradeHint != nil && (tok.UpgradeHint.Message != "" || tok.UpgradeHint.URL != "") {
				list = append(list, NoticeItem{
					ID:       fmt.Sprintf("upgrade-hint-tok-%d", tok.Token),
					Type:     "upgrade_hint",
					Title:    fmt.Sprintf("Account #%d Broadcast", tok.Token+1),
					Message:  tok.UpgradeHint.Message,
					URL:      tok.UpgradeHint.URL,
					Badge:    "Upstream Promo",
					Tone:     "info",
					TokenIdx: &idx,
				})
			}
			if tok.ServerMessage != "" {
				list = append(list, NoticeItem{
					ID:       fmt.Sprintf("server-msg-tok-%d", tok.Token),
					Type:     "server_message",
					Title:    fmt.Sprintf("Account #%d System Message", tok.Token+1),
					Message:  tok.ServerMessage,
					Badge:    "Server Notice",
					Tone:     "warning",
					TokenIdx: &idx,
				})
			}
		}
	}

	return NoticesResponse{
		Notices:   list,
		PeakHours: peak,
		Count:     len(list),
	}
}
