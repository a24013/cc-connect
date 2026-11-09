package telegram

import (
	"testing"

	"github.com/go-telegram/bot/models"
)

func TestCallbackButtonLabel(t *testing.T) {
	msg := &models.Message{ReplyMarkup: &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{{
		{Text: "喝了 250ml", CallbackData: "cmd:/remind_act a1 0"},
		{Text: "30 分鐘後", CallbackData: "cmd:/remind_act a1 snooze"},
	}}}}
	if got := callbackButtonLabel(msg, "cmd:/remind_act a1 snooze", "/remind_act a1 snooze"); got != "30 分鐘後" {
		t.Fatalf("label = %q", got)
	}
	if got := callbackButtonLabel(msg, "cmd:/other", "/other"); got != "/other" {
		t.Fatalf("fallback = %q", got)
	}
	if got := callbackButtonLabel(&models.Message{}, "cmd:/x", "/x"); got != "/x" {
		t.Fatalf("no markup fallback = %q", got)
	}
}
