package bls12_381_gpu

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	curve "github.com/consensys/gnark-crypto/ecc/bls12-381"
	"github.com/consensys/gnark-crypto/ecc/bls12-381/fp"
	"github.com/consensys/gnark-crypto/ecc/bls12-381/fr"
	"github.com/consensys/gnark-crypto/ecc/bls12-381/kzg"

	icicle_core "github.com/ingonyama-zk/icicle-gnark/v3/wrappers/golang/core"
	icicle_bls12_381 "github.com/ingonyama-zk/icicle-gnark/v3/wrappers/golang/curves/bls12381"
	icicle_msm "github.com/ingonyama-zk/icicle-gnark/v3/wrappers/golang/curves/bls12381/msm"
	icicle_ntt "github.com/ingonyama-zk/icicle-gnark/v3/wrappers/golang/curves/bls12381/ntt"
	icicle_vecops "github.com/ingonyama-zk/icicle-gnark/v3/wrappers/golang/curves/bls12381/vecOps"
	icicle_runtime "github.com/ingonyama-zk/icicle-gnark/v3/wrappers/golang/runtime"
	eon_nvtx "github.com/eon-protocol/eonark/gpu/nvtx"
)

func printDeviceAndMemory() {
	if dev, err := icicle_runtime.GetActiveDevice(); err == icicle_runtime.Success && dev != nil {
		fmt.Printf("		[ICICLE] active device: type=%s id=%d\n", dev.GetDeviceType(), dev.Id)
	} else {
		fmt.Printf("		[ICICLE] active device: <unknown> (err=%v)\n", err)
	}
	if mem, err := icicle_runtime.GetAvailableMemory(); err == icicle_runtime.Success && mem != nil {
		used := mem.Total - mem.Free
		pct := 0.0
		if mem.Total > 0 {
			pct = (float64(used) / float64(mem.Total)) * 100.0
		}
		// 显存以 MiB 打印
		fmt.Printf("		[ICICLE] memory: used=%.0f MiB / total=%.0f MiB (%.1f%%)\n",
			float64(used)/1024.0/1024.0, float64(mem.Total)/1024.0/1024.0, pct)
	} else {
		fmt.Printf("		[ICICLE] memory: <unavailable> (err=%v)\n", err)
	}
	// 额外尝试通过 nvidia-smi 打印 GPU 详细信息（包括型号、CUDA版本、计算能力等）
	// 1. GPU 名称和基本信息
	out, smiErr := exec.Command("bash", "-lc", "nvidia-smi --query-gpu=name,driver_version,cuda_version,compute_cap --format=csv,noheader,nounits | head -n1").CombinedOutput()
	if smiErr == nil {
		fields := strings.Split(strings.TrimSpace(string(out)), ",")
		if len(fields) >= 4 {
			name := strings.TrimSpace(fields[0])
			driver := strings.TrimSpace(fields[1])
			cudaVer := strings.TrimSpace(fields[2])
			computeCap := strings.TrimSpace(fields[3])
			fmt.Printf("		[SMI] GPU=%s driver=%s CUDA=%s compute_cap=%s\n", name, driver, cudaVer, computeCap)
		} else {
			fmt.Printf("		[SMI] raw=\"%s\"\n", strings.TrimSpace(string(out)))
		}
	} else {
		fmt.Printf("		[SMI] GPU info unavailable: %v\n", smiErr)
	}
	// 2. GPU 利用率和显存使用
	out2, smiErr2 := exec.Command("bash", "-lc", "nvidia-smi --query-gpu=utilization.gpu,memory.used,memory.total --format=csv,noheader,nounits | head -n1").CombinedOutput()
	if smiErr2 == nil {
		fields := strings.Split(strings.TrimSpace(string(out2)), ",")
		if len(fields) >= 3 {
			util := strings.TrimSpace(fields[0])
			usedMiB := strings.TrimSpace(fields[1])
			totalMiB := strings.TrimSpace(fields[2])
			if u, e1 := strconv.Atoi(util); e1 == nil {
				fmt.Printf("		[SMI] util=%d%% mem=%s/%s MiB\n", u, usedMiB, totalMiB)
			} else {
				fmt.Printf("		[SMI] raw=\"%s\"\n", strings.TrimSpace(string(out2)))
			}
		} else {
			fmt.Printf("		[SMI] raw=\"%s\"\n", strings.TrimSpace(string(out2)))
		}
	} else {
		fmt.Printf("		[SMI] utilization unavailable: %v\n", smiErr2)
	}
}
func blsProjectiveToGnarkAffine(p icicle_bls12_381.Projective) curve.G1Affine {
	bx := p.X.ToBytesLittleEndian()
	by := p.Y.ToBytesLittleEndian()
	bz := p.Z.ToBytesLittleEndian()

	var ax, ay, az fp.Element
	ax, _ = fp.LittleEndian.Element((*[fp.Bytes]byte)(bx))
	ay, _ = fp.LittleEndian.Element((*[fp.Bytes]byte)(by))
	az, _ = fp.LittleEndian.Element((*[fp.Bytes]byte)(bz))

	var zInv fp.Element
	zInv.Inverse(&az)
	ax.Mul(&ax, &zInv)
	ay.Mul(&ay, &zInv)

	return curve.G1Affine{X: ax, Y: ay}
}

// OnDeviceCommit 使用已在 GPU 的 G1 (SRS) 做 MSM： [p]·G1
// p: 多项式系数（假设是 Montgomery 形式；gnark-crypto/fr 默认就是）
// G1Device: 已在设备端的 G1 bases（例如 pk.deviceInfo.G1Device.G1）
// 返回：kzg.Digest (= curve.G1Affine)
func OnDeviceCommit(p []fr.Element, G1Device icicle_core.DeviceSlice) (kzg.Digest, icicle_runtime.EIcicleError) {
	// 0) 验证输入参数
	nScalars := len(p)
	fmt.Printf("		OnDeviceCommit() || 输入验证: nScalars=%d\n", nScalars)
	if nScalars == 0 {
		return kzg.Digest{}, icicle_runtime.InvalidArgument
	}
	
	
	// 1) 把标量拷到设备
	fmt.Printf("		OnDeviceCommit() || host 开始\n")
	host := icicle_core.HostSliceFromElements(p)
	fmt.Printf("		OnDeviceCommit() || host 拷贝到 device 开始 (len=%d)\n", nScalars)
	var scalarsDev icicle_core.DeviceSlice
	host.CopyToDevice(&scalarsDev, true)
	fmt.Printf("		OnDeviceCommit() || host 拷贝到 device 成功\n")
	
	// 1.1) 验证设备端数据长度（如果可能）
	// 注意：DeviceSlice 可能没有直接的 Len() 方法，这里先跳过，在 MSM 调用时验证
	
	// 2) 配置 MSM
	cfg := icicle_msm.GetDefaultMSMConfig()
	// gnark-crypto 的标量/基点默认在 Montgomery 形式
	cfg.AreScalarsMontgomeryForm = true
	cfg.AreBasesMontgomeryForm = false
	if nScalars >= 8388608 {
		cfg.C = 17
	}
	// 确保同步执行（如果配置支持）
	// cfg.IsAsync = false  // 如果存在此选项，设置为 false
	fmt.Printf("		OnDeviceCommit() || MSM 配置成功 (AreScalarsMontgomeryForm=%v, AreBasesMontgomeryForm=%v)\n", 
		cfg.AreScalarsMontgomeryForm, cfg.AreBasesMontgomeryForm)

	// 2.1) 打印当前设备与可用显存/占用情况（如可用则额外查询 nvidia-smi 利用率）
	printDeviceAndMemory()

	// 3) 运行 MSM（输出 1 个 projective 点）
	fmt.Printf("		OnDeviceCommit() || MSM 运行开始 (scalars=%d, 期望 bases>=%d)\n", nScalars, nScalars)
	eon_nvtx.RangePush("BLS12-381: OnDeviceCommit.MSM")
	
	// 3.1) 确保输出缓冲区在设备上（如果 MSM 需要）
	// 注意：根据 icicle 文档，out 可以是 HostSlice，MSM 会自动处理 D2H
	var outDev icicle_core.DeviceSlice
	var outHost icicle_core.HostSlice[icicle_bls12_381.Projective]
	
	// 先尝试使用 HostSlice（标准用法）
	outHost = make(icicle_core.HostSlice[icicle_bls12_381.Projective], 1)
	
	// 记录 MSM 调用前的时间
	msmStartTime := time.Now()
	fmt.Printf("		OnDeviceCommit() || 调用 icicle_msm.Msm() 前 [%s]\n", msmStartTime.Format("15:04:05.000000"))
	fmt.Printf("		OnDeviceCommit() || scalarsDev 已分配，G1Device 已就绪，outHost 已分配\n")
	
	// 调用 MSM（这里可能会阻塞或耗时很长）
	// 注意：MSM 调用可能会阻塞，特别是对于大规模输入（8.4M）
	// 重要：确保在 RunOnDevice 闭包内调用，RunOnDevice 会自动同步
	fmt.Printf("		OnDeviceCommit() || 开始调用 icicle_msm.Msm()，规模=%d\n", nScalars)
	fmt.Printf("		OnDeviceCommit() || 检查：当前是否在 RunOnDevice 闭包内？\n")
	
	// 尝试获取当前设备上下文（用于调试）
	if dev, err := icicle_runtime.GetActiveDevice(); err == icicle_runtime.Success && dev != nil {
		fmt.Printf("		OnDeviceCommit() || 当前活动设备: type=%s id=%d\n", dev.GetDeviceType(), dev.Id)
	} else {
		fmt.Printf("		OnDeviceCommit() || ⚠️  无法获取活动设备: err=%v\n", err)
	}
	
	// // 在 MSM 调用前添加额外的诊断信息和验证
	// fmt.Printf("		OnDeviceCommit() || MSM 调用前诊断: scalarsDev 已分配, G1Device 已就绪, outHost 已分配\n")
	// fmt.Printf("		OnDeviceCommit() || MSM 配置: AreScalarsMontgomeryForm=%v, AreBasesMontgomeryForm=%v\n", 
	// 	cfg.AreScalarsMontgomeryForm, cfg.AreBasesMontgomeryForm)
	
	// // 验证关键参数
	// if nScalars > 0 {
	// 	fmt.Printf("		OnDeviceCommit() || 参数验证: nScalars=%d, 期望 G1Device 长度 >= %d\n", nScalars, nScalars)
	// } else {
	// 	fmt.Printf("		OnDeviceCommit() || ⚠️  警告: nScalars=%d (应该 > 0)\n", nScalars)
	// }
	
	// // 调用 MSM - 这应该是同步调用，会阻塞直到完成
	// fmt.Printf("		OnDeviceCommit() || 执行 icicle_msm.Msm() 调用...\n")
	// fmt.Printf("		OnDeviceCommit() || MSM 调用时间戳: [%s]\n", time.Now().Format("15:04:05.000000"))
	
	// // 记录调用前的内存状态
	// if mem, err := icicle_runtime.GetAvailableMemory(); err == icicle_runtime.Success && mem != nil {
	// 	used := mem.Total - mem.Free
	// 	fmt.Printf("		OnDeviceCommit() || MSM 调用前显存: used=%.0f MiB / total=%.0f MiB (%.1f%%)\n",
	// 		float64(used)/1024.0/1024.0, float64(mem.Total)/1024.0/1024.0, 
	// 		(float64(used)/float64(mem.Total))*100.0)
	// } else {
	// 	fmt.Printf("		OnDeviceCommit() || ⚠️  无法获取调用前显存状态: err=%v\n", err)
	// }
	
	// // 强制刷新输出缓冲区，确保所有日志都被打印
	// os.Stdout.Sync()
	
	// // 最后一次验证：确保所有参数都准备就绪
	// fmt.Printf("		OnDeviceCommit() || 准备调用 MSM: scalars=%d, 配置已设置, 输出缓冲区已分配\n", nScalars)
	// fmt.Printf("		OnDeviceCommit() || ⚠️  即将进入 CUDA kernel，如果卡住请检查 GPU 状态\n")
	
	// 调用 MSM - 这应该是同步调用，会阻塞直到完成
	// 注意：如果这里卡住，可能是 CUDA kernel 死锁或设备驱动问题
	st := icicle_msm.Msm(scalarsDev, G1Device, &cfg, outHost)
	
	// MSM 调用后立即检查状态
	fmt.Printf("		OnDeviceCommit() || icicle_msm.Msm() 调用返回\n")
	fmt.Printf("		OnDeviceCommit() || MSM 返回时间戳: [%s]\n", time.Now().Format("15:04:05.000000"))
	
	// 检查返回状态
	if st != icicle_runtime.Success {
		fmt.Printf("		OnDeviceCommit() || ⚠️  MSM 返回错误状态: %s\n", st.AsString())
		// 记录调用后的内存状态（如果可能）
		if mem, err := icicle_runtime.GetAvailableMemory(); err == icicle_runtime.Success && mem != nil {
			used := mem.Total - mem.Free
			fmt.Printf("		OnDeviceCommit() || MSM 调用后显存: used=%.0f MiB / total=%.0f MiB\n",
				float64(used)/1024.0/1024.0, float64(mem.Total)/1024.0/1024.0)
		}
	} else {
		fmt.Printf("		OnDeviceCommit() || MSM 返回成功状态\n")
	}
	
	msmEndTime := time.Now()
	msmDuration := msmEndTime.Sub(msmStartTime)
	fmt.Printf("		OnDeviceCommit() || 调用 icicle_msm.Msm() 后，状态=%s，耗时=%.6f ms [%s]\n", 
		st.AsString(), float64(msmDuration.Nanoseconds())/1e6, msmEndTime.Format("15:04:05.000000"))
	
	// 如果耗时超过 1 秒，打印警告
	if msmDuration > time.Second {
		fmt.Printf("		OnDeviceCommit() || ⚠️  MSM 耗时较长: %.2f 秒 (规模=%d)\n", 
			msmDuration.Seconds(), nScalars)
	}
	
	eon_nvtx.RangePop()
	
	// 3.2) 检查 MSM 返回状态
	if st != icicle_runtime.Success {
		fmt.Printf("		OnDeviceCommit() || MSM 失败: %s\n", st.AsString())
		_ = scalarsDev.Free()
		return kzg.Digest{}, st
	}
	
	fmt.Printf("		OnDeviceCommit() || MSM 运行成功\n")
	_ = scalarsDev.Free()
	_ = outDev // 如果未使用，忽略

	// 4) 转成 gnark 的 Affine（= kzg.Digest）
	fmt.Printf("		OnDeviceCommit() || 开始转换 Projective -> Affine\n")
	res := blsProjectiveToGnarkAffine(outHost[0])
	fmt.Printf("		OnDeviceCommit() || blsProjectiveToGnarkAffine 成功\n")
	
	fmt.Printf("		OnDeviceCommit() || 完成，返回结果\n")
	return kzg.Digest(res), icicle_runtime.Success
}

func OnDeviceOpen(p []fr.Element, point fr.Element, base icicle_core.DeviceSlice) (kzg.OpeningProof, icicle_runtime.EIcicleError) {
	var proof kzg.OpeningProof

	// 1) 声明值（CPU 做即可，代价可忽略）
	proof.ClaimedValue = eval(p, point)

	// 2) 构造 H(X) = (p(X)-p(point)) / (X-point)
	_p := make([]fr.Element, len(p))
	copy(_p, p)
	h := dividePolyByXminusA(_p, proof.ClaimedValue, point)

	// 3) 对 H 做一次设备端承诺：commit(H)
	//    注意 bases 需要与标量长度一致，这里对子片到 len(h)
	subBase := base.RangeTo(len(h), false)
	dig, st := OnDeviceCommit(h, subBase)
	if st != icicle_runtime.Success {
		return kzg.OpeningProof{}, st
	}

	// 4) 组装返回值
	proof.H = kzg.Digest(dig) // Digest 是 G1Affine 的别名
	return proof, icicle_runtime.Success
}

func eval(p []fr.Element, point fr.Element) fr.Element {
	var res fr.Element
	n := len(p)
	if n == 0 {
		return res // 0
	}
	res.Set(&p[n-1])
	for i := n - 2; i >= 0; i-- {
		res.Mul(&res, &point).Add(&res, &p[i])
	}
	return res
}

func dividePolyByXminusA(f []fr.Element, fa, a fr.Element) []fr.Element {
	// f <- f - f(a)
	f[0].Sub(&f[0], &fa)

	var t fr.Element
	for i := len(f) - 2; i >= 0; i-- {
		t.Mul(&f[i+1], &a)
		f[i].Add(&f[i], &t)
	}
	return f[1:]
}

// 把 fr.Element 变成 icicle NTT 需要的 CosetGen 表示（uint32 limbs*2）
func CosetGenToIcicle(g fr.Element) (out [fr.Limbs * 2]uint32) {
	bits := g.Bits() // [fr.Limbs]uint64
	limbs := icicle_core.ConvertUint64ArrToUint32Arr(bits[:])
	copy(out[:], limbs[:fr.Limbs*2])
	return
}

func INttOnDevice(aDev icicle_core.DeviceSlice) icicle_runtime.EIcicleError {
	cfg := icicle_ntt.GetDefaultNttConfig()
	cfg.Ordering = icicle_core.KNN
	return icicle_ntt.Ntt(aDev, icicle_core.KInverse, &cfg, aDev)
}

// NttOnDevice: 正向NTT（就地 in-place）。如果 isCoset=true 则做 coset-NTT。
// 约定：输入/输出都在 Montgomery 表示。
func NttOnDevice(aDev icicle_core.DeviceSlice) icicle_runtime.EIcicleError {

	cfg := icicle_ntt.GetDefaultNttConfig()
	// KMN = 常用的正向排列（匹配 gnark/icicle 的用法）
	cfg.Ordering = icicle_core.KNN
	return icicle_ntt.Ntt(aDev, icicle_core.KForward, &cfg, aDev)
}

// VecMulOnDevice: 逐元素乘法 acc = acc * other（模 p），就地写回 acc。
// 注意：icicle 的 vecOps 期望“非 Montgomery”表示；如果你的数据现在是 Montgomery，
// 请先调用 MontConvOnDevice(s, false) 转出，再做乘法，必要时乘完再转回。
func VecMulOnDevice(acc, other icicle_core.DeviceSlice) icicle_runtime.EIcicleError {

	vecCfg := icicle_core.DefaultVecOpsConfig()
	return icicle_vecops.VecOp(acc, other, acc, vecCfg, icicle_core.Mul)
}

// MontConvOnDevice: 标量数组的 Montgomery <-> 非Montgomery 转换（就地）
// into=true  => ToMontgomery
// into=false => FromMontgomery
func MontConvOnDevice(s icicle_core.DeviceSlice, into bool) icicle_runtime.EIcicleError {
	if into {
		return icicle_bls12_381.ToMontgomery(s)
	}
	return icicle_bls12_381.FromMontgomery(s)
}
