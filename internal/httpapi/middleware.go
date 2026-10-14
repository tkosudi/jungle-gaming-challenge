package httpapi

import (
	"errors"
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/tkosudi/jungle-gaming-challenge/internal/auth"
)

type principalKey struct{}

// principal devolve a identidade autenticada; só é chamado em rotas protegidas.
func principal(c fiber.Ctx) auth.Principal {
	p, _ := c.Locals(principalKey{}).(auth.Principal)
	return p
}

// requireRole autentica o bearer token e exige o papel informado.
func (a *api) requireRole(role string) fiber.Handler {
	return func(c fiber.Ctx) error {
		p, ok, err := a.authenticate(c)
		if !ok {
			return err
		}
		if !p.HasRole(role) {
			return forbidden(c)
		}
		c.Locals(principalKey{}, p)
		return c.Next()
	}
}

// requireProvider exige o papel de provedor e um provider_id no token: é dele, e somente
// dele, que os handlers tiram o provedor da operação.
func (a *api) requireProvider() fiber.Handler {
	return func(c fiber.Ctx) error {
		p, ok, err := a.authenticate(c)
		if !ok {
			return err
		}
		if !p.HasRole(auth.RoleWageringProvider) || p.ProviderID == "" {
			return forbidden(c)
		}
		c.Locals(principalKey{}, p)
		return c.Next()
	}
}

// authenticate valida "Authorization: Bearer <token>". Quando ok=false a resposta já foi
// escrita (401, ou 503 se as chaves do IdP estão indisponíveis) e err é o retorno do handler.
func (a *api) authenticate(c fiber.Ctx) (auth.Principal, bool, error) {
	scheme, token, found := strings.Cut(c.Get(fiber.HeaderAuthorization), " ")
	token = strings.TrimSpace(token)
	if !found || !strings.EqualFold(scheme, "Bearer") || token == "" {
		c.Set(fiber.HeaderWWWAuthenticate, `Bearer realm="wagering-api"`)
		return auth.Principal{}, false, writeError(c, fiber.StatusUnauthorized, "UNAUTHENTICATED", "a bearer token is required")
	}
	ctx, cancel := requestContext(c)
	defer cancel()
	p, err := a.verifier.Verify(ctx, token)
	switch {
	case errors.Is(err, auth.ErrKeysUnavailable):
		c.Set(fiber.HeaderRetryAfter, "1")
		return auth.Principal{}, false, writeError(c, fiber.StatusServiceUnavailable, "IDP_UNAVAILABLE", "the identity provider is unavailable")
	case err != nil:
		c.Set(fiber.HeaderWWWAuthenticate, `Bearer realm="wagering-api", error="invalid_token"`)
		return auth.Principal{}, false, writeError(c, fiber.StatusUnauthorized, "UNAUTHENTICATED", "invalid or expired token")
	}
	return p, true, nil
}

func forbidden(c fiber.Ctx) error {
	c.Set(fiber.HeaderWWWAuthenticate, `Bearer realm="wagering-api", error="insufficient_scope"`)
	return writeError(c, fiber.StatusForbidden, "FORBIDDEN", "the credentials do not allow this operation")
}
