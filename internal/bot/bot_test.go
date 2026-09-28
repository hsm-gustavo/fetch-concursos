package bot

import (
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestIsAuthorizedUser(t *testing.T) {
	service := &Service{allowedUserID: 6244996803}

	for _, tt := range []struct {
		name string
		user *tgbotapi.User
		want bool
	}{
		{name: "authorized user", user: &tgbotapi.User{ID: 6244996803}, want: true},
		{name: "another user", user: &tgbotapi.User{ID: 1}, want: false},
		{name: "missing user", user: nil, want: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := service.isAuthorizedUser(tt.user); got != tt.want {
				t.Fatalf("isAuthorizedUser() = %v, want %v", got, tt.want)
			}
		})
	}
}
