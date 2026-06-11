package bot

import (
	"strconv"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// Callback data prefixes. Telegram limits callback data to 64 bytes, so option
// answers are encoded by index rather than value.
const (
	cbService = "svc:"
	cbAnswer  = "a:"
	cbConfirm = "confirm:"
)

// serviceKeyboard lists the client's allowed services plus the always-available
// Billing / Account option.
func serviceKeyboard(services []string) tgbotapi.InlineKeyboardMarkup {
	rows := make([][]tgbotapi.InlineKeyboardButton, 0, len(services)+1)
	for _, s := range services {
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(s, cbService+s),
		))
	}
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("Биллинг / Аккаунт", cbService+"Billing"),
	))
	return tgbotapi.NewInlineKeyboardMarkup(rows...)
}

// optionsKeyboard renders a question's options, encoding the answer by index.
func optionsKeyboard(options []string) tgbotapi.InlineKeyboardMarkup {
	rows := make([][]tgbotapi.InlineKeyboardButton, 0, len(options))
	for i, o := range options {
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(o, cbAnswer+strconv.Itoa(i)),
		))
	}
	return tgbotapi.NewInlineKeyboardMarkup(rows...)
}

// confirmKeyboard offers final confirmation or a restart.
func confirmKeyboard() tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("Подтвердить", cbConfirm+"yes"),
			tgbotapi.NewInlineKeyboardButtonData("Начать заново", cbConfirm+"restart"),
		),
	)
}
