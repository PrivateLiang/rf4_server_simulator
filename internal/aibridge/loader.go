package aibridge

// 活动配置快照：Runner、后台复测与状态回读共用的唯一读取路径。
//
// 有活动修订时只认 ai_setting 里的那份快照（地址、模型、修订、凭据引用在
// 单次读取中绑定），再按引用取不可变密文对象；活动引用缺失、指纹不符或
// 解密失败一律锁定，绝不回退同名文件或旧库。
//
// 没有任何活动修订、也没有发布历史时，按旧安装兼容读取：原外部凭据文件
// 优先，旧库 ai_secret 只读后备。这条路径只读，不写库、不改文件。

import (
	"context"
	"errors"
	"fmt"

	"rf4server/internal/aiconfig"
	"rf4server/internal/store"
)

var (
	// ErrConfigLocked 活动配置不可用（缺对象/指纹不符/解密失败/主密钥缺失）：
	// 调用方必须停发并提示锁定，不得回退旧文件。
	ErrConfigLocked = errors.New("ai: 活动配置已锁定")
	// ErrConfigRevisionStale 本次尝试基于的修订已被新的发布取代。
	ErrConfigRevisionStale = errors.New("ai: 活动修订已变化")
)

// Snapshot 是一次读取得到的完整可用配置。
type Snapshot struct {
	Spec          ProviderSpec
	Active        aiconfig.ActiveConfig
	Legacy        bool
	ProviderID    int64
	CredentialRef string
	Masked        string
}

// Revision 返回快照对应的活动修订；旧安装为 0。
func (s Snapshot) Revision() int64 { return s.Active.Revision }

// LoadSnapshot 读取当前活动配置并解析出可发送的 ProviderSpec。
func LoadSnapshot(ctx context.Context, db *store.DB, masterKeyFile string, masterKey []byte) (Snapshot, error) {
	if db == nil {
		return Snapshot{}, errors.New("ai: 数据库未连接")
	}
	base, err := db.AIConfigSnapshot(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	return ResolveSnapshot(ctx, db, base, masterKeyFile, masterKey)
}

// ResolveSnapshot loads credentials for the supplied, already bound database snapshot.
func ResolveSnapshot(ctx context.Context, db *store.DB, base store.AIConfigSnapshot, masterKeyFile string, masterKey []byte) (Snapshot, error) {
	s := Snapshot{
		Legacy:        base.Legacy,
		ProviderID:    base.ProviderID,
		CredentialRef: base.CredentialRef,
		Active: aiconfig.ActiveConfig{
			Schema: aiconfig.ConfigSchemaVersion, Revision: base.Revision,
			ProviderID: base.ProviderID, ProviderName: base.ProviderName,
			Protocol: base.Protocol, AuthScheme: base.AuthScheme,
			BaseURL: base.BaseURL, ModelRowID: base.ModelRowID, ModelID: base.ModelID,
			ConfigJSON: base.ConfigJSON, CredentialRef: base.CredentialRef,
		},
		Spec: ProviderSpec{
			Name: base.ProviderName, Protocol: base.Protocol, BaseURL: base.BaseURL,
			AuthScheme: base.AuthScheme, ModelID: base.ModelID, ConfigJSON: base.ConfigJSON,
		},
	}
	if base.Legacy {
		// 旧安装：按原优先级解析（外部凭据文件 → 旧库密文），只读。
		plain, _, rerr := ResolveProviderAPIKey(ctx, db, CredentialsPath(masterKeyFile), masterKey, base.ProviderName)
		if rerr != nil {
			return Snapshot{}, fmt.Errorf("%w: %w", ErrConfigLocked, rerr)
		}
		s.Spec.APIKey = plain
		return s, nil
	}
	obj, plain, rerr := ResolveCredentialObject(
		CredentialDir(masterKeyFile), base.CredentialRef, masterKey, base.ProviderID)
	if rerr != nil {
		return Snapshot{}, fmt.Errorf("%w: %w", ErrConfigLocked, rerr)
	}
	s.Spec.APIKey = plain
	s.Masked = obj.Masked
	return s, nil
}

// ActiveRevision 单次读取活动修订号；没有活动指针返回 0。
// 读失败或记录损坏返回错误（不能当成未初始化）。
func ActiveRevision(ctx context.Context, db *store.DB) (int64, error) {
	raw, found, err := db.AIConfigActiveRaw(ctx)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, nil
	}
	active, ok := aiconfig.ParseActive(raw)
	if !ok {
		return 0, store.ErrAIConfigSnapshotInvalid
	}
	return active.Revision, nil
}

// StaleConfigError 表示发送前的活动修订已不是本次尝试读取的修订。
// 调用方应丢弃这次尝试，下一次重新取整套快照。
type StaleConfigError struct {
	Want int64
	Have int64
}

func (e *StaleConfigError) Error() string {
	return fmt.Sprintf("ai: 活动修订已从 %d 变为 %d，丢弃本次尝试", e.Want, e.Have)
}

func (e *StaleConfigError) Unwrap() error { return ErrConfigRevisionStale }

// Gate validates configuration for synchronous callers. Runtime paths use the
// captured intent and exact credential reference via GateIntent.
func Gate(ctx context.Context, db *store.DB, revision int64) error {
	return gate(ctx, db, 0, revision, "", false)
}

func GateIntent(ctx context.Context, db *store.DB, epoch int64, snap Snapshot) error {
	return gate(ctx, db, epoch, snap.Revision(), snap.CredentialRef, true)
}

func gate(ctx context.Context, db *store.DB, epoch, revision int64, ref string, bound bool) error {
	reading, current, err := db.AIGateConfiguration(ctx)
	if err != nil {
		return err
	}
	dec := aiconfig.EvaluateEmergency(reading)
	if !bound {
		epoch = dec.Epoch
	}
	if active, reason := dec.IntentActive(epoch); !active {
		return &EgressDeniedError{State: aiconfig.StateEmergency, Display: "关（" + reason + "）"}
	}
	if current.Revision != revision || (bound && current.CredentialRef != ref) {
		return &StaleConfigError{Want: revision, Have: current.Revision}
	}
	return nil
}
