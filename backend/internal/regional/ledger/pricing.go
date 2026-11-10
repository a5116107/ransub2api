package ledger

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/regional"
)

// Price is an immutable contract for either text tokens or one video task.
// Text rates are nano USD per million tokens, with disjoint input categories.
// Adapters must normalize terminal provider evidence before settlement. Tools,
// audio, images and service tiers are not represented by either contract.
type Price struct {
	Model         string      `json:"model"`
	Input         int64       `json:"input_nano_per_million"`
	Output        int64       `json:"output_nano_per_million"`
	CacheCreation int64       `json:"cache_creation_nano_per_million"`
	CacheRead     int64       `json:"cache_read_nano_per_million"`
	MaxInput      int64       `json:"max_input_tokens"`
	MaxOutput     int64       `json:"max_output_tokens"`
	Video         *VideoPrice `json:"video,omitempty"`
}

func (p Price) Validate() error {
	if p.Video != nil {
		if p.Model == "" || p.Input != 0 || p.Output != 0 || p.CacheCreation != 0 || p.CacheRead != 0 || p.MaxInput != 0 || p.MaxOutput != 0 {
			return errors.New("video and text pricing contracts cannot be mixed")
		}
		return p.Video.validate()
	}
	if p.Model == "" || p.Input < 0 || p.Output < 0 || p.CacheCreation < 0 || p.CacheRead < 0 || p.MaxInput <= 0 || p.MaxOutput <= 0 || p.MaxInput > 2147483647 || p.MaxOutput > 2147483647 {
		return errors.New("invalid bounded text pricing contract")
	}
	return nil
}

// Version is content addressed; modifying any rate or limit changes identity.
func (p Price) Version() string {
	raw, _ := json.Marshal(p)
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func (p Price) Charge(input, output, creation, read int64) (regional.Amount, error) {
	if err := p.Validate(); err != nil {
		return 0, err
	}
	if p.Video != nil {
		return 0, errors.New("video settlement requires terminal task evidence")
	}
	if input < 0 || output < 0 || creation < 0 || read < 0 || input > p.MaxInput || creation > p.MaxInput-input || read > p.MaxInput-input-creation || output > p.MaxOutput {
		return 0, errors.New("usage exceeds pricing contract bounds")
	}
	sum := new(big.Int)
	for _, pair := range [][2]int64{{input, p.Input}, {output, p.Output}, {creation, p.CacheCreation}, {read, p.CacheRead}} {
		sum.Add(sum, new(big.Int).Mul(big.NewInt(pair[0]), big.NewInt(pair[1])))
	}
	return roundedPrice(sum, big.NewInt(1_000_000))
}

// Round UP once to the primary NUMERIC(20,8) quantum, never per category or
// before applying the sale multiplier. All monetary calculations stay exact.
func roundedPrice(numerator, denominator *big.Int) (regional.Amount, error) {
	quantum := new(big.Int).Mul(denominator, big.NewInt(10))
	value := new(big.Int).Add(numerator, new(big.Int).Sub(quantum, big.NewInt(1)))
	value.Quo(value, quantum)
	value.Mul(value, big.NewInt(10))
	if !value.IsInt64() {
		return 0, errors.New("price exceeds ledger amount range")
	}
	return regional.Amount(value.Int64()), nil
}

// Maximum reserves the entire model input window, not a character/token
// estimate. Cache writes may be more expensive than ordinary input.
func (p Price) Maximum(output int64) (regional.Amount, error) {
	if p.Video != nil {
		return 0, errors.New("text output limits cannot bound video work")
	}
	if output <= 0 || output > p.MaxOutput {
		return 0, errors.New("explicit bounded output limit required")
	}
	ceiling := p
	ceiling.Input = max(p.Input, p.CacheCreation, p.CacheRead)
	return ceiling.Charge(p.MaxInput, output, 0, 0)
}

// VideoRequest freezes the requested billing dimensions; a status response is
// never allowed to replace them with an MP4 length or another resolution.
type VideoRequest struct {
	Provider        string `json:"provider"`
	UpstreamModel   string `json:"upstream_model"`
	Resolution      string `json:"resolution,omitempty"`
	DurationSeconds int64  `json:"duration_seconds,omitempty"`
}

func (v VideoRequest) validate() error {
	if v.UpstreamModel == "" || v.UpstreamModel != strings.TrimSpace(v.UpstreamModel) || v.DurationSeconds < 0 || v.DurationSeconds > 2147483647 {
		return errors.New("invalid video request dimensions")
	}
	switch v.Provider {
	case "minimax-h3":
		if v.UpstreamModel != "MiniMax-H3" || v.Resolution != "768p" || v.DurationSeconds < 1 || v.DurationSeconds > 15 {
			return errors.New("invalid MiniMax-H3 price dimensions")
		}
	case "grok":
		if v.DurationSeconds < 1 || (v.Resolution != "480p" && v.Resolution != "720p" && v.Resolution != "1080p") {
			return errors.New("invalid Grok video price dimensions")
		}
	case "seedance":
		if v.Resolution != "" && v.Resolution != "480p" && v.Resolution != "720p" && v.Resolution != "1080p" {
			return errors.New("invalid Seedance video price dimensions")
		}
	default:
		return errors.New("unsupported video price provider")
	}
	return nil
}

// VideoPrice certifies one request's selected tariff, upstream identity and
// rational sale multiplier. UnitNano is per request, second or million terminal
// completion tokens according to Basis; it is not an upstream purchase price.
type VideoPrice struct {
	VideoRequest
	Basis               string `json:"basis"`
	UnitNano            int64  `json:"unit_nano_usd"`
	SaleNumerator       int64  `json:"sale_numerator"`
	SaleDenominator     int64  `json:"sale_denominator"`
	MaxCompletionTokens int64  `json:"max_completion_tokens,omitempty"`
	TemplateVersion     string `json:"template_version,omitempty"`
}

func (v VideoPrice) validate() error {
	if err := v.VideoRequest.validate(); err != nil {
		return err
	}
	if v.UnitNano < 0 || v.SaleNumerator < 0 || v.SaleDenominator <= 0 || new(big.Int).GCD(nil, nil, big.NewInt(v.SaleNumerator), big.NewInt(v.SaleDenominator)).Cmp(big.NewInt(1)) != 0 {
		return errors.New("invalid video tariff or noncanonical sale multiplier")
	}
	if v.TemplateVersion != "" && !validDigest(v.TemplateVersion) {
		return errors.New("invalid video template version")
	}
	if v.Provider == "seedance" {
		if v.Basis != "completion_tokens" || v.MaxCompletionTokens <= 0 || v.MaxCompletionTokens > 2147483647 {
			return errors.New("seedance requires bounded terminal completion-token pricing")
		}
	} else if v.MaxCompletionTokens != 0 || (v.Basis != "second" && (v.Provider != "minimax-h3" || v.Basis != "request")) {
		return errors.New("video provider does not support this tariff basis")
	}
	return nil
}

// DeriveVideoPrice composes the configured sale tariff with the effective SQL
// sale policy exactly once. The immutable parent proves the base tariff; text
// identities and historical video prices retain their original JSON/version.
func DeriveVideoPrice(template Price, multiplier *big.Rat) (Price, error) {
	if template.Validate() != nil || template.Video == nil || template.Video.TemplateVersion != "" || multiplier == nil || multiplier.Sign() < 0 {
		return Price{}, ErrConflict
	}
	rate := new(big.Rat).Mul(new(big.Rat).SetFrac64(template.Video.SaleNumerator, template.Video.SaleDenominator), multiplier)
	if !rate.Num().IsInt64() || !rate.Denom().IsInt64() {
		return Price{}, errors.New("video sale multiplier exceeds contract range")
	}
	p := template
	v := *template.Video
	p.Video = &v
	v.TemplateVersion = template.Version()
	v.SaleNumerator, v.SaleDenominator = rate.Num().Int64(), rate.Denom().Int64()
	return p, p.Validate()
}

// VideoUsage contains a validated provider terminal state. Unknown, queued or
// interrupted submissions cannot be used to refund or settle a reservation.
type VideoUsage struct {
	VideoRequest
	TaskID           string `json:"task_id"`
	State            string `json:"state"`
	CompletionTokens int64  `json:"completion_tokens,omitempty"`
}

func (v VideoUsage) validate() error {
	if err := v.VideoRequest.validate(); err != nil {
		return err
	}
	if !validVideoTaskID(v.TaskID) || v.CompletionTokens < 0 || v.CompletionTokens > 2147483647 {
		return errors.New("invalid video terminal evidence")
	}
	switch v.State {
	case "completed":
		if v.Provider != "seedance" && v.CompletionTokens != 0 {
			return errors.New("video tariff does not bill completion tokens")
		}
	case "failed", "cancelled", "expired":
		if v.CompletionTokens != 0 {
			return errors.New("failed video has unresolved billed usage")
		}
	default:
		return errors.New("video terminal state missing; hold retained")
	}
	return nil
}

func validVideoTaskID(id string) bool {
	return id != "" && id == strings.TrimSpace(id) && len(id) <= 256 && !strings.ContainsAny(id, "\x00\r\n")
}

func (p Price) VideoCosts(v VideoUsage) (base, charge regional.Amount, err error) {
	if err = p.Validate(); err != nil {
		return 0, 0, err
	}
	if p.Video == nil || p.Video.VideoRequest != v.VideoRequest {
		return 0, 0, ErrConflict
	}
	if err = v.validate(); err != nil {
		return 0, 0, err
	}
	if v.CompletionTokens > p.Video.MaxCompletionTokens {
		return 0, 0, errors.New("video completion tokens exceed reserved bound")
	}
	if v.State != "completed" {
		return 0, 0, nil
	}
	return p.Video.costs(v.CompletionTokens)
}

func (v VideoPrice) costs(completionTokens int64) (base, charge regional.Amount, err error) {
	quantity, denominator := int64(1), int64(1)
	switch v.Basis {
	case "second":
		quantity = v.DurationSeconds
	case "completion_tokens":
		quantity, denominator = completionTokens, 1_000_000
	}
	numerator := new(big.Int).Mul(big.NewInt(v.UnitNano), big.NewInt(quantity))
	base, err = roundedPrice(numerator, big.NewInt(denominator))
	if err != nil {
		return 0, 0, err
	}
	charge, err = roundedPrice(new(big.Int).Mul(numerator, big.NewInt(v.SaleNumerator)), new(big.Int).Mul(big.NewInt(denominator), big.NewInt(v.SaleDenominator)))
	return base, charge, err
}

func (p Price) VideoMaximum(v VideoRequest) (regional.Amount, error) {
	if err := p.Validate(); err != nil {
		return 0, err
	}
	if p.Video == nil || p.Video.VideoRequest != v {
		return 0, ErrConflict
	}
	_, charge, err := p.Video.costs(p.Video.MaxCompletionTokens)
	return charge, err
}

// AccountMaximum reserves the account's base-cost quota, independently of the
// discounted (or free) downstream sale. The price comes from this SQL authority.
func AccountMaximum(ctx context.Context, tx *sql.Tx, r Reservation) (regional.Amount, error) {
	if r.Video == nil {
		return r.Maximum, nil
	}
	p, err := ReadPrice(ctx, tx, r.PricingVersion, r.Model)
	if err != nil || p.Video == nil || p.Video.VideoRequest != *r.Video {
		return 0, errors.Join(ErrConflict, err)
	}
	base, charge, err := p.Video.costs(p.Video.MaxCompletionTokens)
	if err != nil || charge != r.Maximum {
		return 0, errors.Join(ErrConflict, err)
	}
	return base, nil
}

// AccountCharge is independently recomputed from immutable terminal evidence;
// no regional caller or sale multiplier can declare the account quota debit.
func AccountCharge(ctx context.Context, tx *sql.Tx, e Event) (regional.Amount, error) {
	if e.Video == nil {
		return e.Usage.Charge, nil
	}
	p, err := ReadPrice(ctx, tx, e.PricingVersion, e.Model)
	if err != nil {
		return 0, err
	}
	base, charge, err := p.VideoCosts(*e.Video)
	if err != nil || charge != e.Usage.Charge {
		return 0, errors.Join(ErrConflict, err)
	}
	return base, nil
}

type priceStore interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func InstallPrice(ctx context.Context, db priceStore, p Price) error {
	if err := p.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `INSERT INTO regional_prices(version,price) VALUES($1,$2) ON CONFLICT DO NOTHING`, p.Version(), string(raw))
	if err != nil {
		return err
	}
	var old []byte
	if err = db.QueryRowContext(ctx, `SELECT price FROM regional_prices WHERE version=$1`, p.Version()).Scan(&old); err != nil {
		return err
	}
	if !sameJSON(old, raw) {
		return ErrConflict
	}
	return nil
}

// Video reservations must match the installed immutable dimensions and full
// ceiling. Text zero holds still require every billable category to be free.
func verifyPriceReservation(ctx context.Context, tx *sql.Tx, r Reservation) error {
	if err := r.validateVideoOrigin(); err != nil {
		return err
	}
	if r.Maximum != 0 && r.Video == nil {
		return nil
	}
	p, err := ReadPrice(ctx, tx, r.PricingVersion, r.Model)
	if err != nil {
		return err
	}
	if r.Video != nil {
		maximum, err := p.VideoMaximum(*r.Video)
		if err != nil || maximum != r.Maximum {
			return ErrConflict
		}
	} else if p.Video != nil || p.Input != 0 || p.Output != 0 || p.CacheCreation != 0 || p.CacheRead != 0 {
		return ErrConflict
	}
	return nil
}

// ReadPrice verifies the stored content hash, model and bounded tariff before
// returning an authority-owned immutable contract.
func ReadPrice(ctx context.Context, reader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, version, model string) (Price, error) {
	var raw []byte
	var p Price
	if err := reader.QueryRowContext(ctx, `SELECT price FROM regional_prices WHERE version=$1`, version).Scan(&raw); err != nil {
		return p, err
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, err
	}
	if p.Validate() != nil || p.Version() != version || p.Model != model {
		return p, ErrConflict
	}
	return p, nil
}

func verifyVideoSettlement(ctx context.Context, tx *sql.Tx, r Reservation, e Event) error {
	if r.Video == nil && e.Video == nil {
		return nil
	}
	if !videoMatchesReservation(r, e) {
		return ErrConflict
	}
	if err := e.validateVideo(); err != nil {
		return err
	}
	p, err := ReadPrice(ctx, tx, r.PricingVersion, r.Model)
	if err != nil {
		return err
	}
	_, charge, err := p.VideoCosts(*e.Video)
	if err != nil {
		return err
	}
	if charge != e.Usage.Charge {
		return ErrConflict
	}
	return nil
}

// PricingVerifier independently loads authority-owned pricing, validates the
// final amount and inserts the canonical usage row within Primary.Settle's
// transaction. Scope MUST verify durable admission and all quota dimensions;
// current account snapshots alone are insufficient evidence. Balance was already
// debited at grant issuance and is intentionally not charged a second time.
// This verifier is not automatically installed on the public application.
func PricingVerifier(scope VerifyEvent) VerifyEvent {
	return func(ctx context.Context, tx *sql.Tx, e Event) error {
		if scope == nil {
			return errors.New("durable admission scope verifier required")
		}
		p, err := ReadPrice(ctx, tx, e.PricingVersion, e.Model)
		if err != nil {
			return err
		}
		var base, charge regional.Amount
		billingMode, count, multiplier := "token", 0, "1"
		var resolution, duration any
		if p.Video == nil {
			if e.Video != nil {
				return ErrConflict
			}
			charge, err = p.Charge(e.InputTokens, e.OutputTokens, e.CacheCreationTokens, e.CacheReadTokens)
			base = charge
		} else {
			if e.Video == nil {
				return ErrConflict
			}
			if err := e.validateVideo(); err != nil {
				return err
			}
			base, charge, err = p.VideoCosts(*e.Video)
			if p.Video.Resolution != "" {
				resolution = p.Video.Resolution
			}
			if p.Video.DurationSeconds > 0 {
				duration = p.Video.DurationSeconds
			}
			multiplier = new(big.Rat).SetFrac(big.NewInt(p.Video.SaleNumerator), big.NewInt(p.Video.SaleDenominator)).FloatString(18)
			if p.Video.Provider != "seedance" {
				billingMode = "video"
				if e.Video.State == "completed" {
					count = 1
				}
			}
		}
		if err != nil {
			return err
		}
		if charge != e.Usage.Charge {
			return ErrConflict
		}
		if err = scope(ctx, tx, e); err != nil {
			return err
		}
		amount, err := USD(charge)
		if err != nil {
			return err
		}
		baseAmount, err := USD(base)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO usage_logs(user_id,api_key_id,account_id,request_id,model,input_tokens,output_tokens,cache_creation_tokens,cache_read_tokens,total_cost,actual_cost,billing_mode,video_count,video_resolution,video_duration_seconds,rate_multiplier,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::numeric,$11::numeric,$12,$13,$14,$15,$16::numeric,$17)`, e.Usage.UserID, e.Usage.APIKeyID, e.Usage.AccountID, e.Usage.EventID, e.Model, e.InputTokens, e.OutputTokens, e.CacheCreationTokens, e.CacheReadTokens, baseAmount, amount, billingMode, count, resolution, duration, multiplier, e.Usage.CompletedAt)
		return err
	}
}
