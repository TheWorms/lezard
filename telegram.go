package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"
)

// telegramAPI est remplaçable dans les tests.
var telegramAPI = "https://api.telegram.org"

var tgClient = &http.Client{Timeout: 15 * time.Second}

// tgError distingue les refus définitifs (chat introuvable, bot bloqué, jeton
// refusé : inutile de réessayer) des pannes passagères (réseau, 429, 5xx).
type tgError struct {
	msg       string
	permanent bool
	network   bool
}

func (e *tgError) Error() string { return e.msg }

func isPermanent(err error) bool {
	var te *tgError
	return errors.As(err, &te) && te.permanent
}

func isNetwork(err error) bool {
	var te *tgError
	return errors.As(err, &te) && te.network
}

// tgCall appelle une méthode de l'API Bot. Le jeton n'apparaît jamais dans les erreurs.
func tgCall(token, method string, form url.Values, result any) error {
	resp, err := tgClient.PostForm(telegramAPI+"/bot"+token+"/"+method, form)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // retire l'URL, qui contient le jeton
		}
		return &tgError{msg: fmt.Sprintf("Telegram injoignable : %v", err), network: true}
	}
	defer resp.Body.Close()
	var r struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&r); err != nil {
		return &tgError{msg: fmt.Sprintf("réponse de Telegram illisible (HTTP %d)", resp.StatusCode), network: true}
	}
	if !r.OK {
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusNotFound:
			return &tgError{msg: "jeton refusé par Telegram", permanent: true}
		case http.StatusBadRequest, http.StatusForbidden:
			return &tgError{msg: "Telegram a refusé la demande : " + r.Description, permanent: true}
		}
		// 429 (trop de messages), 5xx : panne passagère, on réessaiera au prochain passage
		return &tgError{msg: "Telegram a refusé la demande : " + r.Description, network: true}
	}
	if result != nil {
		return json.Unmarshal(r.Result, result)
	}
	return nil
}

func telegramSend(token, chat, text string) error {
	return tgCall(token, "sendMessage", url.Values{"chat_id": {chat}, "text": {text}, "disable_web_page_preview": {"true"}}, nil)
}

// telegramBotName renvoie @nom_du_bot ; sert aussi à vérifier le jeton.
func telegramBotName(token string) (string, error) {
	var me struct {
		Username string `json:"username"`
	}
	if err := tgCall(token, "getMe", nil, &me); err != nil {
		return "", err
	}
	return "@" + me.Username, nil
}

type tgChat struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Title string `json:"title"`
}

// telegramChats liste les conversations qui ont récemment écrit au bot
// (ex. /start), pour éviter de chercher l'identifiant à la main.
func telegramChats(token string) ([]tgChat, error) {
	type chat struct {
		ID        int64  `json:"id"`
		Type      string `json:"type"`
		Title     string `json:"title"`
		FirstName string `json:"first_name"`
		LastName  string `json:"last_name"`
		Username  string `json:"username"`
	}
	type msg struct {
		Chat chat `json:"chat"`
	}
	var updates []struct {
		Message           *msg `json:"message"`
		ChannelPost       *msg `json:"channel_post"`
		MyChatMember      *msg `json:"my_chat_member"`
		EditedMessage     *msg `json:"edited_message"`
		EditedChannelPost *msg `json:"edited_channel_post"`
	}
	if err := tgCall(token, "getUpdates", url.Values{"limit": {"100"}, "timeout": {"0"}}, &updates); err != nil {
		return nil, err
	}
	seen := map[int64]tgChat{}
	for _, u := range updates {
		for _, m := range []*msg{u.Message, u.ChannelPost, u.MyChatMember, u.EditedMessage, u.EditedChannelPost} {
			if m == nil || m.Chat.ID == 0 {
				continue
			}
			c := m.Chat
			title := c.Title
			if title == "" {
				title = c.FirstName
				if c.LastName != "" {
					title += " " + c.LastName
				}
			}
			if c.Username != "" {
				title += " (@" + c.Username + ")"
			}
			kind := map[string]string{"private": "personne", "group": "groupe", "supergroup": "groupe", "channel": "canal"}[c.Type]
			seen[c.ID] = tgChat{ID: strconv.FormatInt(c.ID, 10), Kind: kind, Title: title}
		}
	}
	out := make([]tgChat, 0, len(seen))
	for _, c := range seen {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Title < out[j].Title })
	return out, nil
}
