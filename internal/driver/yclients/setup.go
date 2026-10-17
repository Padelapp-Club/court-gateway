package yclients

import "github.com/Padelapp-Club/court-gateway/internal/driver"

func init() {
	driver.RegisterSetup("yclients", driver.SetupSpec{
		Name:  "YCLIENTS",
		Needs: "партнёрский токен YCLIENTS и пользовательский токен (или логин и пароль сотрудника филиала)",
		Fields: []driver.SetupField{
			{Key: "login", Flag: "crm-login", Env: "CRM_LOGIN", Prompt: "Логин сотрудника YCLIENTS (пусто — если есть пользовательский токен)"},
			{Key: "api_url", Flag: "crm-api-url"},
		},
		Secrets: []driver.SetupSecret{
			{Env: PartnerTokenEnv, Prompt: "Партнёрский токен YCLIENTS", Required: true},
			{Env: UserTokenEnv, Prompt: "Пользовательский токен YCLIENTS (пусто — войти логином и паролем)"},
			{Env: "CRM_PASSWORD", Prompt: "Пароль сотрудника YCLIENTS (пусто — если задан пользовательский токен)"},
		},
	})
}
