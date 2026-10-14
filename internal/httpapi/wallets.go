package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"strconv"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"

	"github.com/tkosudi/jungle-gaming-challenge/internal/domain"
)

const (
	defaultLedgerLimit = 50
	maxLedgerLimit     = 100
)

type openWalletRequest struct {
	PlayerID       string     `json:"playerId"`
	InitialBalance *moneyJSON `json:"initialBalance"`
}

type walletResponse struct {
	ID       string    `json:"id"`
	PlayerID string    `json:"playerId"`
	Balance  moneyJSON `json:"balance"`
	Version  int64     `json:"version"`
}

func toWalletResponse(w domain.Wallet) walletResponse {
	return walletResponse{ID: w.ID.String(), PlayerID: w.PlayerID.String(), Balance: toMoneyJSON(w.Balance), Version: w.Version}
}

// openWallet: 201 com a carteira criada; 409 se o jogador já tem carteira nessa moeda.
func (a *api) openWallet(c fiber.Ctx) error {
	var req openWalletRequest
	if ok, err := decodeJSON(c, &req); !ok {
		return err
	}
	playerID, err := uuid.Parse(req.PlayerID)
	if err != nil {
		return validationError(c, "playerId must be a UUID")
	}
	if req.InitialBalance == nil {
		return validationError(c, "initialBalance is required")
	}
	initial, err := domain.NewMoney(req.InitialBalance.Amount, req.InitialBalance.Currency)
	if err != nil {
		return validationError(c, err.Error())
	}

	ctx, cancel := requestContext(c)
	defer cancel()
	res, err := a.service.OpenWallet(ctx, playerID, initial)
	if err != nil {
		return serviceError(c, err)
	}
	if !res.Created {
		return writeError(c, fiber.StatusConflict, "WALLET_ALREADY_EXISTS", "the player already has a wallet in this currency")
	}
	c.Set(fiber.HeaderLocation, "/wallets/"+res.Wallet.ID.String())
	return c.Status(fiber.StatusCreated).JSON(toWalletResponse(res.Wallet))
}

func (a *api) getWallet(c fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("walletId"))
	if err != nil {
		return validationError(c, "walletId must be a UUID")
	}
	ctx, cancel := requestContext(c)
	defer cancel()
	w, err := a.service.GetWallet(ctx, id)
	if err != nil {
		return serviceError(c, err)
	}
	return c.JSON(toWalletResponse(w))
}

// ledgerCursor é opaco para o cliente (base64url de JSON). Carrega a carteira para que um
// cursor não seja reaproveitado em outra, e a última versão entregue: a ordem é a da versão.
type ledgerCursor struct {
	WalletID string `json:"w"`
	Version  int64  `json:"v"`
}

func encodeCursor(cur ledgerCursor) string {
	raw, _ := json.Marshal(cur) // struct de campos simples: Marshal não falha
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeCursor(s string) (ledgerCursor, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return ledgerCursor{}, false
	}
	var cur ledgerCursor
	if err := json.Unmarshal(raw, &cur); err != nil || cur.Version < 0 {
		return ledgerCursor{}, false
	}
	return cur, true
}

type ledgerEntryResponse struct {
	ID            string    `json:"id"`
	TransactionID string    `json:"transactionId"`
	Direction     string    `json:"direction"`
	Money         moneyJSON `json:"money"`
	BalanceBefore moneyJSON `json:"balanceBefore"`
	BalanceAfter  moneyJSON `json:"balanceAfter"`
	WalletVersion int64     `json:"walletVersion"`
	CreatedAt     string    `json:"createdAt"`
}

type ledgerResponse struct {
	WalletID   string                `json:"walletId"`
	Entries    []ledgerEntryResponse `json:"entries"`
	NextCursor *string               `json:"nextCursor"`
}

func (a *api) listLedger(c fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("walletId"))
	if err != nil {
		return validationError(c, "walletId must be a UUID")
	}
	limit := defaultLedgerLimit
	if raw := c.Query("limit"); raw != "" {
		if limit, err = strconv.Atoi(raw); err != nil || limit < 1 || limit > maxLedgerLimit {
			return validationError(c, "limit must be an integer between 1 and 100")
		}
	}
	var after int64
	if raw := c.Query("cursor"); raw != "" {
		cur, ok := decodeCursor(raw)
		if !ok || cur.WalletID != id.String() {
			return validationError(c, "cursor is invalid for this wallet")
		}
		after = cur.Version
	}

	ctx, cancel := requestContext(c)
	defer cancel()
	entries, more, err := a.service.ListLedger(ctx, id, after, limit)
	if err != nil {
		return serviceError(c, err)
	}
	resp := ledgerResponse{WalletID: id.String(), Entries: make([]ledgerEntryResponse, 0, len(entries))}
	for _, e := range entries {
		resp.Entries = append(resp.Entries, ledgerEntryResponse{
			ID: e.ID.String(), TransactionID: e.TransactionID.String(), Direction: string(e.Direction),
			Money: toMoneyJSON(e.Amount), BalanceBefore: toMoneyJSON(e.BalanceBefore), BalanceAfter: toMoneyJSON(e.BalanceAfter),
			WalletVersion: e.WalletVersion, CreatedAt: timestamp(e.At),
		})
	}
	if more {
		next := encodeCursor(ledgerCursor{WalletID: id.String(), Version: entries[len(entries)-1].WalletVersion})
		resp.NextCursor = &next
	}
	return c.JSON(resp)
}

type reconciliationResponse struct {
	WalletID          string    `json:"walletId"`
	StoredBalance     moneyJSON `json:"storedBalance"`
	CalculatedBalance moneyJSON `json:"calculatedBalance"`
	Difference        moneyJSON `json:"difference"`
	Consistent        bool      `json:"consistent"`
	CheckedEntries    int64     `json:"checkedEntries"`
}

// reconcile compara o saldo armazenado com o reconstruído pelo ledger. É POST porque executa
// uma verificação (com log e métrica), mas não altera saldo nem ledger.
func (a *api) reconcile(c fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("walletId"))
	if err != nil {
		return validationError(c, "walletId must be a UUID")
	}
	ctx, cancel := requestContext(c)
	defer cancel()
	r, err := a.service.Reconcile(ctx, id)
	if err != nil {
		return serviceError(c, err)
	}
	return c.JSON(reconciliationResponse{
		WalletID: r.WalletID.String(), StoredBalance: toMoneyJSON(r.StoredBalance),
		CalculatedBalance: toMoneyJSON(r.CalculatedBalance), Difference: toMoneyJSON(r.Difference),
		Consistent: r.Consistent, CheckedEntries: r.CheckedEntries,
	})
}
