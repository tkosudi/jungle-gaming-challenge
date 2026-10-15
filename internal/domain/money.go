// Package domain contém as regras financeiras puras: sem Fx, Fiber, pgx ou SQS.
package domain

import (
	"errors"
	"fmt"
	"math"
	"strings"
)

var (
	ErrInvalidAmount    = errors.New("domain: invalid amount")
	ErrNegativeAmount   = errors.New("domain: negative amount")
	ErrAmountOverflow   = errors.New("domain: amount overflow")
	ErrInvalidCurrency  = errors.New("domain: invalid currency")
	ErrCurrencyMismatch = errors.New("domain: currency mismatch")
)

// Currency é um código ISO 4217 aceito pelo sistema.
type Currency string

// supportedCurrencies reúne os códigos ISO 4217 ativos com duas casas decimais:
// o contrato fixa a escala em duas casas, então moedas com 0 ou 3 casas (JPY, KWD)
// não são representáveis e ficam de fora, assim como códigos de teste (XXX).
var supportedCurrencies = makeCurrencySet(
	"AED AFN ALL AMD ANG AOA ARS AUD AWG AZN BAM BBD BDT BGN BMD BND BOB BRL BSD BTN BWP BYN BZD " +
		"CAD CDF CHF CNY COP CRC CUP CVE CZK DKK DOP DZD EGP ERN ETB EUR FJD FKP GBP GEL GHS GIP GMD " +
		"GTQ GYD HKD HNL HTG HUF IDR ILS INR IRR JMD KES KGS KHR KPW KYD KZT LAK LBP LKR LRD LSL MAD " +
		"MDL MGA MKD MMK MNT MOP MRU MUR MVR MWK MXN MYR MZN NAD NGN NIO NOK NPR NZD PAB PEN PGK PHP " +
		"PKR PLN QAR RON RSD RUB SAR SBD SCR SDG SEK SGD SHP SLE SOS SRD SSP STN SVC SYP SZL THB TJS " +
		"TMT TOP TRY TTD TWD TZS UAH USD UYU UZS VES WST XCD YER ZAR ZMW ZWG")

func makeCurrencySet(codes string) map[Currency]struct{} {
	set := make(map[Currency]struct{})
	for _, code := range strings.Fields(codes) {
		set[Currency(code)] = struct{}{}
	}
	return set
}

// ParseCurrency valida o código exatamente como informado (maiúsculas, sem normalização).
func ParseCurrency(code string) (Currency, error) {
	c := Currency(code)
	if _, ok := supportedCurrencies[c]; !ok {
		return "", fmt.Errorf("%w: %q", ErrInvalidCurrency, code)
	}
	return c, nil
}

// Money é um valor imutável em unidades mínimas (centavos) com moeda explícita.
// Valores negativos existem apenas como resultado de aritmética interna (diferenças, como a
// da reconciliação): NewMoney e MoneyFromCents recusam negativos, e a carteira só aceita
// movimentações positivas e nunca fica abaixo de zero. Soma, subtração e negação detectam
// overflow do int64. O zero value (sem moeda) é inválido e rejeitado por todas as operações.
type Money struct {
	cents    int64
	currency Currency
}

// NewMoney converte a string decimal do contrato ("25.00") e valida a moeda.
// Exige exatamente duas casas, sem sinal, espaço, separador de milhar ou notação científica.
func NewMoney(amount, currency string) (Money, error) {
	c, err := ParseCurrency(currency)
	if err != nil {
		return Money{}, err
	}
	cents, err := parseCents(amount)
	if err != nil {
		return Money{}, err
	}
	return Money{cents: cents, currency: c}, nil
}

// MoneyFromCents reconstrói um valor persistido.
func MoneyFromCents(cents int64, currency Currency) (Money, error) {
	if _, ok := supportedCurrencies[currency]; !ok {
		return Money{}, fmt.Errorf("%w: %q", ErrInvalidCurrency, string(currency))
	}
	if cents < 0 {
		return Money{}, ErrNegativeAmount
	}
	return Money{cents: cents, currency: currency}, nil
}

// MoneyFromSignedCents reconstrói um resultado interno que pode ser negativo, como o saldo
// reconstruído pela reconciliação de um ledger corrompido. Nunca para entradas externas nem
// para saldos de carteira, que usam NewMoney e MoneyFromCents.
func MoneyFromSignedCents(cents int64, currency Currency) (Money, error) {
	if _, ok := supportedCurrencies[currency]; !ok {
		return Money{}, fmt.Errorf("%w: %q", ErrInvalidCurrency, string(currency))
	}
	return Money{cents: cents, currency: currency}, nil
}

func parseCents(s string) (int64, error) {
	if strings.HasPrefix(s, "-") {
		return 0, fmt.Errorf("%w: %q", ErrNegativeAmount, s)
	}
	dot := strings.IndexByte(s, '.')
	if dot <= 0 || len(s)-dot-1 != 2 {
		return 0, fmt.Errorf("%w: %q must have an integer part and exactly two decimals", ErrInvalidAmount, s)
	}
	whole, fraction := s[:dot], s[dot+1:]
	if len(whole) > 1 && whole[0] == '0' {
		return 0, fmt.Errorf("%w: %q has leading zeros", ErrInvalidAmount, s)
	}

	var cents int64
	for _, digits := range [...]string{whole, fraction} {
		for i := 0; i < len(digits); i++ {
			d := int64(digits[i]) - '0'
			if d < 0 || d > 9 {
				return 0, fmt.Errorf("%w: %q", ErrInvalidAmount, s)
			}
			if cents > (math.MaxInt64-d)/10 {
				return 0, fmt.Errorf("%w: %q", ErrAmountOverflow, s)
			}
			cents = cents*10 + d
		}
	}
	return cents, nil
}

func (m Money) Cents() int64       { return m.cents }
func (m Money) Currency() Currency { return m.currency }

// Amount formata o valor como "123.45" usando apenas aritmética inteira.
func (m Money) Amount() string {
	sign, abs := "", uint64(m.cents)
	if m.cents < 0 {
		// uint64(-x) com x = MinInt64 continua correto: o valor absoluto cabe em uint64.
		sign, abs = "-", uint64(-(m.cents+1))+1
	}
	return fmt.Sprintf("%s%d.%02d", sign, abs/100, abs%100)
}

func (m Money) String() string { return m.Amount() + " " + string(m.currency) }

func (m Money) IsZero() bool     { return m.currency != "" && m.cents == 0 }
func (m Money) IsPositive() bool { return m.currency != "" && m.cents > 0 }
func (m Money) IsNegative() bool { return m.currency != "" && m.cents < 0 }

func (m Money) Add(o Money) (Money, error) {
	if err := m.compatible(o); err != nil {
		return Money{}, err
	}
	if (o.cents > 0 && m.cents > math.MaxInt64-o.cents) || (o.cents < 0 && m.cents < math.MinInt64-o.cents) {
		return Money{}, ErrAmountOverflow
	}
	return Money{cents: m.cents + o.cents, currency: m.currency}, nil
}

func (m Money) Sub(o Money) (Money, error) {
	if err := m.compatible(o); err != nil {
		return Money{}, err
	}
	if (o.cents < 0 && m.cents > math.MaxInt64+o.cents) || (o.cents > 0 && m.cents < math.MinInt64+o.cents) {
		return Money{}, ErrAmountOverflow
	}
	return Money{cents: m.cents - o.cents, currency: m.currency}, nil
}

// Neg devolve o valor com o sinal invertido; -MinInt64 não cabe em int64 e é overflow.
func (m Money) Neg() (Money, error) {
	if m.currency == "" {
		return Money{}, ErrInvalidCurrency
	}
	if m.cents == math.MinInt64 {
		return Money{}, ErrAmountOverflow
	}
	return Money{cents: -m.cents, currency: m.currency}, nil
}

// Compare devolve -1, 0 ou 1.
func (m Money) Compare(o Money) (int, error) {
	if err := m.compatible(o); err != nil {
		return 0, err
	}
	switch {
	case m.cents < o.cents:
		return -1, nil
	case m.cents > o.cents:
		return 1, nil
	}
	return 0, nil
}

func (m Money) compatible(o Money) error {
	if m.currency == "" || o.currency == "" {
		return ErrInvalidCurrency
	}
	if m.currency != o.currency {
		return fmt.Errorf("%w: %s vs %s", ErrCurrencyMismatch, m.currency, o.currency)
	}
	return nil
}
