//go:build icicle

package gpu

import (
	"fmt"
	"sync"

	"github.com/consensys/gnark-crypto/ecc/bls12-381/fr"
	"github.com/consensys/gnark-crypto/ecc/bls12-381/fr/fft"

	"github.com/consensys/gnark-crypto/ecc/bls12-381/kzg"
	plonkbls12381 "github.com/consensys/gnark/backend/plonk/bls12-381"

	icicle_core "github.com/ingonyama-zk/icicle-gnark/v3/wrappers/golang/core"
	icicle_bls12_381 "github.com/ingonyama-zk/icicle-gnark/v3/wrappers/golang/curves/bls12381"
	icicle_msm "github.com/ingonyama-zk/icicle-gnark/v3/wrappers/golang/curves/bls12381/msm"
	icicle_runtime "github.com/ingonyama-zk/icicle-gnark/v3/wrappers/golang/runtime"
)

type deviceInfo struct {
	Device   icicle_runtime.Device
	G1Device struct {
		G1         icicle_core.DeviceSlice
		G1Lagrange icicle_core.DeviceSlice
	}

	N             int
	CosetTable    icicle_core.DeviceSlice // not Montgomery
	CosetTableRev icicle_core.DeviceSlice // not Montgomery

	// 大域Twiddles，用于scalingVector, 长度为n
	BigTwiddlesN    icicle_core.DeviceSlice // [1, w_N, w_N^2, ...]
	BigTwiddlesNRev icicle_core.DeviceSlice // 位反序版

	// —— 仅用于 CPU 回退时的"按需"Host 表（懒加载）
	onceCoset, onceBig         sync.Once
	hostCosetReg, hostCosetRev []fr.Element
	hostBigReg, hostBigRev     []fr.Element
	// 供构建 big 表使用的生成元（setup 时记下）
	bigW fr.Element

	// MSM 预计算相关字段
	// Lagrange bases 的预计算结果（用于 L/R/O 等多项式的 commit）
	// 注意：这些字段在 setupDevicePointers 中通过 initMsmPrecomputeLag 初始化
	G1LagPrecomp  icicle_core.DeviceSlice
	hasLagPrecomp bool                  // 标记是否已初始化预计算
	MsmCfgLag     icicle_core.MSMConfig // Lagrange bases 的 MSM 配置

	// Monomial bases 的预计算结果（用于普通 KZG commit）
	// 注意：这些字段在 setupDevicePointers 中通过 initMsmPrecomputeG1 初始化
	G1Precomp    icicle_core.DeviceSlice
	hasG1Precomp bool                  // 标记是否已初始化预计算
	MsmCfgG1     icicle_core.MSMConfig // Monomial bases 的 MSM 配置

	mu sync.Mutex
}

// 同名结构，icicle 版多了 deviceInfo
type ProvingKey struct {
	Kzg         kzg.ProvingKey
	KzgLagrange kzg.ProvingKey
	Vk          *plonkbls12381.VerifyingKey
	deviceInfo  *deviceInfo
}

func WrapProvingKey(pk *plonkbls12381.ProvingKey) (*ProvingKey, error) {
	warmUpDevice()
	return &ProvingKey{
		Kzg:         pk.Kzg,
		KzgLagrange: pk.KzgLagrange,
		Vk:          pk.Vk,
		deviceInfo:  &deviceInfo{},
	}, nil
}

func (di *deviceInfo) ensureHostCosetTables(d0 *fft.Domain) ([]fr.Element, []fr.Element) {
	di.onceCoset.Do(func() {
		tab, _ := d0.CosetTable() // [1, s, s^2, ...] ，s = d1.FrMultiplicativeGen 由 gnark 内部管理
		di.hostCosetReg = tab
		di.hostCosetRev = make([]fr.Element, len(tab))
		copy(di.hostCosetRev, tab)
		fft.BitReverse(di.hostCosetRev)
	})
	return di.hostCosetReg, di.hostCosetRev
}

func (di *deviceInfo) ensureHostBigTables(n uint64) ([]fr.Element, []fr.Element) {
	di.onceBig.Do(func() {
		reg := make([]fr.Element, n)
		if n > 0 {
			reg[0].SetOne()
			if n > 1 {
				reg[1].Set(&di.bigW)
				for i := 2; i < int(n); i++ {
					reg[i].Mul(&reg[i-1], &di.bigW)
				}
			}
		}
		di.hostBigReg = reg
		di.hostBigRev = make([]fr.Element, len(reg))
		copy(di.hostBigRev, reg)
		fft.BitReverse(di.hostBigRev)
	})
	return di.hostBigReg, di.hostBigRev
}

// initMsmPrecomputeLag 对 Lagrange bases 做一次预计算，存储到 G1LagPrecomp
// N: 多项式长度（例如 domain0.Cardinality）
func (di *deviceInfo) initMsmPrecomputeLag(N int) error {
	cfg := icicle_msm.GetDefaultMSMConfig()
	cfg.AreScalarsMontgomeryForm = true
	cfg.AreBasesMontgomeryForm = false
	cfg.ArePointsSharedInBatch = true
	cfg.IsAsync = false

	// 根据 N 选择 precompute_factor 和 c
	// 只对大 MSM 开启预计算（N >= 2^21）
	if N >= 512 { // N >= 2^21
		if N >= 8388608 { // N >= 2^23，包括 8388610 等
			cfg.PrecomputeFactor = 3
			// cfg.C = 20
		} else if N >= 4194304 { // 2^22 <= N < 2^23
			cfg.PrecomputeFactor = 2 // 不要用 4，否则会 OOM + fallback
			// cfg.C = 14
		} else { // 2^21 <= N < 2^22
			cfg.PrecomputeFactor = 5
		}
	} else {
		// 小规模 MSM，不使用预计算
		// 直接使用原始 bases，避免重复存储
		cfg.PrecomputeFactor = 1
		cfg.C = 0
		di.MsmCfgLag = cfg
		di.hasLagPrecomp = false // 标记为未预计算，使用原始 bases
		return nil
	}

	// 只有大规模 MSM 才进行预计算
	var sample icicle_bls12_381.Affine
	precomputeSize := N * int(cfg.PrecomputeFactor)

	var precomputeErr icicle_runtime.EIcicleError
	done := make(chan struct{})
	icicle_runtime.RunOnDevice(&di.Device, func(args ...any) {
		defer close(done)
		if _, st := di.G1LagPrecomp.Malloc(sample.Size(), precomputeSize); st != icicle_runtime.Success {
			precomputeErr = st
			return
		}

		base := di.G1Device.G1Lagrange.RangeTo(N, false)
		if st := icicle_msm.PrecomputeBases(base, &cfg, di.G1LagPrecomp); st != icicle_runtime.Success {
			precomputeErr = st
			return
		}
	})
	<-done

	if precomputeErr != icicle_runtime.Success {
		return fmt.Errorf("initMsmPrecomputeLag failed: %s", precomputeErr.AsString())
	}

	di.MsmCfgLag = cfg
	di.hasLagPrecomp = true
	return nil
}

// initMsmPrecomputeG1 对 Monomial bases 做一次预计算，存储到 G1Precomp
// N: 多项式长度（例如 domain0.Cardinality）
// 注意：考虑到 quotient 多项式 h1/h2/h3 的长度可能是 N+2 或 N+3，预计算时使用 N+3 以确保覆盖
func (di *deviceInfo) initMsmPrecomputeG1(N int) error {
	cfg := icicle_msm.GetDefaultMSMConfig()
	cfg.AreScalarsMontgomeryForm = true
	cfg.AreBasesMontgomeryForm = false
	cfg.ArePointsSharedInBatch = true
	cfg.IsAsync = false

	// 根据 N 选择 precompute_factor 和 c
	// 只对大 MSM 开启预计算（N >= 2^21）
	if N >= 512 { // N >= 2^21
		if N > 8388608 {
			cfg.PrecomputeFactor = 1
		} else if N == 8388608 { // N >= 2^23 （含 8388610）
			// quotient 阶段 GPU 已经很满了，这里不要再用预计算，避免 fallback 到 sequential
			cfg.PrecomputeFactor = 1
			// cfg.C = 16
		} else if N >= 4194304 { // 2^22 <= N < 2^23
			cfg.PrecomputeFactor = 2
		} else { // 2^21 <= N < 2^22
			cfg.PrecomputeFactor = 6
		}
	} else {
		// 小规模 MSM，不使用预计算
		// 直接使用原始 bases，避免重复存储
		cfg.PrecomputeFactor = 1
		cfg.C = 0
		di.MsmCfgG1 = cfg
		di.hasG1Precomp = false // 标记为未预计算，使用原始 bases
		return nil
	}

	// 只有大规模 MSM 才进行预计算
	// 预计算 N+3 个点，以覆盖 h1/h2/h3 的最大可能长度（N+2 或 N+3）
	// 注意：实际预计算大小仍然是 (N+3) * PrecomputeFactor
	maxPolyLen := N + 3
	var sample icicle_bls12_381.Affine
	precomputeSize := maxPolyLen * int(cfg.PrecomputeFactor)

	var precomputeErr icicle_runtime.EIcicleError
	done := make(chan struct{})
	icicle_runtime.RunOnDevice(&di.Device, func(args ...any) {
		defer close(done)
		if _, st := di.G1Precomp.Malloc(sample.Size(), precomputeSize); st != icicle_runtime.Success {
			precomputeErr = st
			return
		}

		// 使用前 maxPolyLen 个 bases 进行预计算
		base := di.G1Device.G1.RangeTo(maxPolyLen, false)
		if st := icicle_msm.PrecomputeBases(base, &cfg, di.G1Precomp); st != icicle_runtime.Success {
			precomputeErr = st
			return
		}
	})
	<-done

	if precomputeErr != icicle_runtime.Success {
		return fmt.Errorf("initMsmPrecomputeG1 failed: %s", precomputeErr.AsString())
	}

	di.MsmCfgG1 = cfg
	di.hasG1Precomp = true
	return nil
}
