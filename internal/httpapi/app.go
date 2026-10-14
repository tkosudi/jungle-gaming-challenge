// Package httpapi expõe a camada de transporte HTTP baseada em Fiber v3.
package httpapi

import "github.com/gofiber/fiber/v3"

// NewApp cria a aplicação Fiber com as rotas públicas de saúde.
func NewApp() *fiber.App {
	app := fiber.New()
	app.Get("/health/live", live)
	return app
}

func live(c fiber.Ctx) error {
	return c.JSON(fiber.Map{"status": "ok"})
}
