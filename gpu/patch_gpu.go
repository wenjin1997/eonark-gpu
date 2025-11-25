//go:build icicle

package gpu

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/big"
	"math/bits"
	"runtime"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/consensys/gnark-crypto/ecc"

	curve "github.com/consensys/gnark-crypto/ecc/bls12-381"

	"github.com/consensys/gnark-crypto/ecc/bls12-381/fr"

	"github.com/consensys/gnark-crypto/ecc/bls12-381/fr/fft"
	"github.com/consensys/gnark-crypto/ecc/bls12-381/fr/iop"
	"github.com/consensys/gnark-crypto/ecc/bls12-381/fr/poseidon2"

	"github.com/consensys/gnark-crypto/ecc/bls12-381/kzg"
	"github.com/consensys/gnark/backend"
	"github.com/consensys/gnark/backend/witness"

	"github.com/consensys/gnark/constraint"
	cs "github.com/consensys/gnark/constraint/bls12-381"
	"github.com/consensys/gnark/constraint/solver"
	fcs "github.com/consensys/gnark/frontend/cs"

	plonkbls12381 "github.com/consensys/gnark/backend/plonk/bls12-381"
	"github.com/consensys/gnark/logger"

	// eon "github.com/eon-protocol/eonark"
	kzg_bls12_381 "github.com/eon-protocol/eonark/gpu/bls12381"
	nvtx "github.com/eon-protocol/eonark/gpu/nvtx"
	eon "github.com/eon-protocol/eonark/zkcore"

	icicle_core "github.com/ingonyama-zk/icicle-gnark/v3/wrappers/golang/core"
	icicle_bls12_381 "github.com/ingonyama-zk/icicle-gnark/v3/wrappers/golang/curves/bls12381"
	icicle_ntt "github.com/ingonyama-zk/icicle-gnark/v3/wrappers/golang/curves/bls12381/ntt"
	icicle_runtime "github.com/ingonyama-zk/icicle-gnark/v3/wrappers/golang/runtime"
)

const HasIcicle = true

const (
	id_L int = iota
	id_R
	id_O
	id_Z
	id_ZS
	id_Ql
	id_Qr
	id_Qm
	id_Qo
	id_Qk
	id_S1
	id_S2
	id_S3
	id_Qci // [ .. , Qc_i, Pi_i, ...]
)

// blinding factors
const (
	id_Bl int = iota
	id_Br
	id_Bo
	id_Bz
	nb_blinding_polynomials
)

// blinding orders (-1 to deactivate)
const (
	order_blinding_L = 1
	order_blinding_R = 1
	order_blinding_O = 1
	order_blinding_Z = 2
)

// jade参考：memory layout + communication的cost（打点测一下copy的总时间+分布时间）
func (pk *ProvingKey) setupDevicePointers(spr *cs.SparseR1CS) error {
	// ① 选择/创建后端 & 设备
	if st := icicle_runtime.LoadBackendFromEnvOrDefault(); st != icicle_runtime.Success {
		return fmt.Errorf("icicle backend: %s", st.AsString())
	}
	dev := icicle_runtime.CreateDevice("CUDA", 0)
	pk.deviceInfo = &deviceInfo{Device: dev}

	d0 := fft.NewDomain(uint64(spr.GetNbConstraints() + len(spr.Public)))
	n := int(d0.Cardinality)

	if len(pk.Kzg.G1) < n+3 || len(pk.KzgLagrange.G1) < n {
		return errors.New("CK or LK not compatible with the circuit size")
	}

	/*************************  G1 Device Setup ***************************/
	// ② 在 device 上完成拷贝和 Montgomery 变换
	var copyErr error
	done := make(chan struct{})
	icicle_runtime.RunOnDevice(&pk.deviceInfo.Device, func(args ...any) {
		defer close(done)

		g1Host := icicle_core.HostSlice[curve.G1Affine](pk.Kzg.G1)
		g1Host.CopyToDevice(&pk.deviceInfo.G1Device.G1, true)

		g1LagHost := icicle_core.HostSlice[curve.G1Affine](pk.KzgLagrange.G1)
		g1LagHost.CopyToDevice(&pk.deviceInfo.G1Device.G1Lagrange, true)

		if st := icicle_bls12_381.AffineFromMontgomery(pk.deviceInfo.G1Device.G1); st != icicle_runtime.Success {
			copyErr = fmt.Errorf("AffineFromMontgomery(G1): %s", st.AsString())
			return
		}
		if st := icicle_bls12_381.AffineFromMontgomery(pk.deviceInfo.G1Device.G1Lagrange); st != icicle_runtime.Success {
			copyErr = fmt.Errorf("AffineFromMontgomery(G1Lagrange): %s", st.AsString())
			return
		}
	})
	<-done
	if copyErr != nil {
		return copyErr
	}

	/***********************  Host 侧预计算  **************************/
	// —— 小域 twiddles / twiddlesInv（长度 = n）, 直接调用InitDomain（用小域 d0 的原根）
	genBits := d0.Generator.Bits()
	limbs := icicle_core.ConvertUint64ArrToUint32Arr(genBits[:])
	var rou icicle_bls12_381.ScalarField
	rou = rou.FromLimbs(limbs)

	var stRls icicle_runtime.EIcicleError
	var stInit icicle_runtime.EIcicleError
	done = make(chan struct{})
	icicle_runtime.RunOnDevice(&pk.deviceInfo.Device, func(args ...any) {
		defer close(done)
		stRls = icicle_ntt.ReleaseDomain()
		stInit = icicle_ntt.InitDomain(rou, icicle_core.GetDefaultNTTInitDomainConfig())
	})
	<-done
	if stRls != icicle_runtime.Success {
		return fmt.Errorf("ReleaseDomain failed: %s", stRls.AsString())
	}
	if stInit != icicle_runtime.Success {
		return fmt.Errorf("InitDomain failed: %s", stInit.AsString())
	}
	pk.deviceInfo.N = n

	// —— 生成 cosetTable, cosetTableInv（长度 = n）, 以及它的位反序版本cosetTableRev
	var d1 *fft.Domain
	if d0.Cardinality < 6 {
		d1 = fft.NewDomain(8*d0.Cardinality, fft.WithoutPrecompute())
	} else {
		d1 = fft.NewDomain(4*d0.Cardinality, fft.WithoutPrecompute())
	}

	// cosetShift 取大域的 FrMultiplicativeGen（与 computeNumerator 的第一块一致）
	cosetShift := d1.FrMultiplicativeGen

	cos := make([]fr.Element, n) // [1, s, s², ...]
	cos[0].SetOne()
	if n > 1 {
		cos[1].Set(&cosetShift)
		for i := 2; i < n; i++ {
			cos[i].Mul(&cos[i-1], &cosetShift)
		}
	}

	// coset 的位反序版本
	cosRev := make([]fr.Element, n)
	copy(cosRev, cos)
	fft.BitReverse(cosRev)

	// —— 大域 w^j 幂表（长度 = n），以及它的位反序版本
	bigTwiddles := make([]fr.Element, n)
	bigW := d1.Generator
	fft.BuildExpTable(bigW, bigTwiddles)

	bigRevTwiddles := make([]fr.Element, n)
	copy(bigRevTwiddles, bigTwiddles)
	fft.BitReverse(bigRevTwiddles)

	/***********************  上传到显存（并转非 Mont）  **************************/
	done = make(chan struct{})
	icicle_runtime.RunOnDevice(&pk.deviceInfo.Device, func(args ...any) {
		defer close(done)

		// —— cosetTable
		hCos := icicle_core.HostSliceFromElements(cos)
		hCos.CopyToDevice(&pk.deviceInfo.CosetTable, true)

		// —— cosetTableRev
		hCosRev := icicle_core.HostSliceFromElements(cosRev)
		hCosRev.CopyToDevice(&pk.deviceInfo.CosetTableRev, true)

		// 统一转为“非 Montgomery”，便于后续 VecMulOnDevice 直接使用
		if st := kzg_bls12_381.MontConvOnDevice(pk.deviceInfo.CosetTable, false); st != icicle_runtime.Success {
			copyErr = fmt.Errorf("FromMontgomery(cosetTable): %s", st.AsString())
			return
		}
		if st := kzg_bls12_381.MontConvOnDevice(pk.deviceInfo.CosetTableRev, false); st != icicle_runtime.Success {
			copyErr = fmt.Errorf("FromMontgomery(cosetTableRev): %s", st.AsString())
			return
		}

		// —— big twiddles
		hBig := icicle_core.HostSliceFromElements(bigTwiddles)
		hBig.CopyToDevice(&pk.deviceInfo.BigTwiddlesN, true)

		// —— big twiddles rev
		hBigRev := icicle_core.HostSliceFromElements(bigRevTwiddles)
		hBigRev.CopyToDevice(&pk.deviceInfo.BigTwiddlesNRev, true)

		// 统一转为“非 Montgomery”，便于后续 VecMulOnDevice 直接使用
		if st := kzg_bls12_381.MontConvOnDevice(pk.deviceInfo.BigTwiddlesN, false); st != icicle_runtime.Success {
			copyErr = fmt.Errorf("FromMontgomery(bigTwiddlesN): %s", st.AsString())
			return
		}
		if st := kzg_bls12_381.MontConvOnDevice(pk.deviceInfo.BigTwiddlesNRev, false); st != icicle_runtime.Success {
			copyErr = fmt.Errorf("FromMontgomery(bigTwiddlesNRev): %s", st.AsString())
			return
		}

		// 供cpu回退懒加载使用
		pk.deviceInfo.bigW = bigW

	})
	<-done
	if copyErr != nil {
		return copyErr
	}

	return nil

}

func hostFromFrSlice(v []fr.Element) icicle_core.HostSlice[fr.Element] {
	return icicle_core.HostSliceFromElements(v)
}

func prove(spr *cs.SparseR1CS, pk *ProvingKey, fullWitness witness.Witness, opts ...backend.ProverOption) (*plonkbls12381.Proof, error) {

	if HasIcicle {
		start := time.Now()
		if err := pk.setupDevicePointers(spr); err != nil {
			return nil, fmt.Errorf("icicle device setup: %w", err)
		}
		elapsed := time.Since(start)
		fmt.Printf("prove() -> icicle device setup 耗时: %.6fms\n", float64(elapsed.Nanoseconds())/1e6)
	}

	log := logger.Logger().With().
		Str("curve", spr.CurveID().String()).
		Int("nbConstraints", spr.GetNbConstraints()).
		Str("backend", "plonk").Logger()

	// parse the options
	opt, err := backend.NewProverConfig(opts...)
	if err != nil {
		return nil, fmt.Errorf("get prover options: %w", err)
	}

	start := time.Now()
	start_prove_time := time.Now()

	// init instance
	g, ctx := errgroup.WithContext(context.Background())
	instance, err := newInstance(ctx, spr, pk, fullWitness, &opt)
	if err != nil {
		return nil, fmt.Errorf("new instance: %w", err)
	}

	// solve constraints
	g.Go(func() error {
		nvtx.RangePush("prove: solveConstraints")
		defer nvtx.RangePop()
		start_solve_constraints_time := time.Now()
		err := instance.solveConstraints()
		elapsed_solve_constraints_time := time.Since(start_solve_constraints_time)
		fmt.Printf("prove() -> solveConstraints 耗时: %.6fms\n", float64(elapsed_solve_constraints_time.Nanoseconds())/1e6)
		return err
	})

	// complete qk
	g.Go(func() error {
		nvtx.RangePush("prove: completeQk")
		defer nvtx.RangePop()
		start_complete_qk_time := time.Now()
		err := instance.completeQk()
		elapsed_complete_qk_time := time.Since(start_complete_qk_time)
		fmt.Printf("prove() -> completeQk 耗时: %.6fms\n", float64(elapsed_complete_qk_time.Nanoseconds())/1e6)
		return err
	})

	// init blinding polynomials
	g.Go(func() error {
		nvtx.RangePush("prove: initBlindingPolynomials")
		defer nvtx.RangePop()
		start_init_blinding_polynomials_time := time.Now()
		err := instance.initBlindingPolynomials()
		elapsed_init_blinding_polynomials_time := time.Since(start_init_blinding_polynomials_time)
		fmt.Printf("prove() -> initBlindingPolynomials 耗时: %.6fms\n", float64(elapsed_init_blinding_polynomials_time.Nanoseconds())/1e6)
		return err
	})

	// derive gamma, beta (copy constraint)
	g.Go(func() error {
		nvtx.RangePush("prove: deriveGammaAndBeta")
		defer nvtx.RangePop()
		start_derive_gamma_and_beta_time := time.Now()
		err := instance.deriveGammaAndBeta()
		elapsed_derive_gamma_and_beta_time := time.Since(start_derive_gamma_and_beta_time)
		fmt.Printf("prove() -> deriveGammaAndBeta 耗时: %.6fms\n", float64(elapsed_derive_gamma_and_beta_time.Nanoseconds())/1e6)
		return err
	})

	// compute accumulating ratio for the copy constraint
	g.Go(func() error {
		nvtx.RangePush("prove: buildRatioCopyConstraint")
		defer nvtx.RangePop()
		start_build_ratio_copy_constraint_time := time.Now()
		err := instance.buildRatioCopyConstraint()
		elapsed_build_ratio_copy_constraint_time := time.Since(start_build_ratio_copy_constraint_time)
		fmt.Printf("prove() -> buildRatioCopyConstraint 耗时: %.6fms\n", float64(elapsed_build_ratio_copy_constraint_time.Nanoseconds())/1e6)
		return err
	})

	// compute h
	g.Go(func() error {
		nvtx.RangePush("prove: computeQuotient")
		defer nvtx.RangePop()
		start_compute_quotient_time := time.Now()
		err := instance.computeQuotient()
		elapsed_compute_quotient_time := time.Since(start_compute_quotient_time)
		fmt.Printf("prove() -> computeQuotient 耗时: %.6fms\n", float64(elapsed_compute_quotient_time.Nanoseconds())/1e6)
		return err
	})

	// open Z (blinded) at ωζ (proof.ZShiftedOpening)
	g.Go(func() error {
		nvtx.RangePush("prove: openZ")
		defer nvtx.RangePop()
		start_open_z_time := time.Now()
		err := instance.openZ()
		elapsed_open_z_time := time.Since(start_open_z_time)
		fmt.Printf("prove() -> openZ 耗时: %.6fms\n", float64(elapsed_open_z_time.Nanoseconds())/1e6)
		return err
	})

	// linearized polynomial
	g.Go(func() error {
		nvtx.RangePush("prove: computeLinearizedPolynomial")
		defer nvtx.RangePop()
		start_compute_linearized_polynomial_time := time.Now()
		err := instance.computeLinearizedPolynomial()
		elapsed_compute_linearized_polynomial_time := time.Since(start_compute_linearized_polynomial_time)
		fmt.Printf("prove() -> computeLinearizedPolynomial 耗时: %.6fms\n", float64(elapsed_compute_linearized_polynomial_time.Nanoseconds())/1e6)
		return err
	})

	// Batch opening
	g.Go(func() error {
		nvtx.RangePush("prove: batchOpening")
		defer nvtx.RangePop()
		start_batch_opening_time := time.Now()
		err := instance.batchOpening()
		elapsed_batch_opening_time := time.Since(start_batch_opening_time)
		fmt.Printf("prove() -> batchOpening 耗时: %.6fms\n", float64(elapsed_batch_opening_time.Nanoseconds())/1e6)
		return err
	})

	if err := g.Wait(); err != nil {
		return nil, err
	}

	elapsed_prove_time := time.Since(start_prove_time)
	fmt.Printf("prove() -> prove() 总耗时: %.6fms\n", float64(elapsed_prove_time.Nanoseconds())/1e6)

	log.Debug().Dur("took", time.Since(start)).Msg("prover done")
	return instance.proof, nil
}

// represents a Prover instance
type instance struct {
	ctx context.Context

	pk    *ProvingKey
	proof *plonkbls12381.Proof
	spr   *cs.SparseR1CS
	opt   *backend.ProverConfig

	fs *Transcript

	// polynomials
	x                         []*iop.Polynomial // x stores tracks the polynomial we need
	bp                        []*iop.Polynomial // blinding polynomials
	h                         *iop.Polynomial   // h is the quotient polynomial
	blindedZ                  []fr.Element      // blindedZ is the blinded version of Z
	quotientShardsRandomizers [2]fr.Element     // random elements for blinding the shards of the quotient

	precomputedDenominators    []fr.Element // stores the denominators of the Lagrange polynomials
	linearizedPolynomial       []fr.Element
	linearizedPolynomialDigest kzg.Digest

	fullWitness witness.Witness

	// bsb22 commitment stuff
	commitmentInfo constraint.PlonkCommitments
	commitmentVal  []fr.Element
	cCommitments   []*iop.Polynomial

	// challenges
	gamma, beta, alpha, zeta fr.Element

	// channel to wait for the steps
	chLRO,
	chQk,
	chbp,
	chZ,
	chH,
	chRestoreLRO,
	chZOpening,
	chLinearizedPolynomial,
	chGammaBeta chan struct{}

	domain0, domain1 *fft.Domain

	trace *plonkbls12381.Trace
}

func newInstance(ctx context.Context, spr *cs.SparseR1CS, pk *ProvingKey, fullWitness witness.Witness, opts *backend.ProverConfig) (*instance, error) {
	s := instance{
		ctx:                    ctx,
		pk:                     pk,
		proof:                  &plonkbls12381.Proof{},
		spr:                    spr,
		opt:                    opts,
		fullWitness:            fullWitness,
		bp:                     make([]*iop.Polynomial, nb_blinding_polynomials),
		fs:                     NewTranscript(eon.CID_GAMMA, eon.CID_BETA, eon.CID_ALPHA, eon.CID_ZETA),
		chLRO:                  make(chan struct{}, 1),
		chQk:                   make(chan struct{}, 1),
		chbp:                   make(chan struct{}, 1),
		chGammaBeta:            make(chan struct{}, 1),
		chZ:                    make(chan struct{}, 1),
		chH:                    make(chan struct{}, 1),
		chZOpening:             make(chan struct{}, 1),
		chLinearizedPolynomial: make(chan struct{}, 1),
		chRestoreLRO:           make(chan struct{}, 1),
	}
	s.initBSB22Commitments()
	s.x = make([]*iop.Polynomial, id_Qci+2*len(s.commitmentInfo))

	// init fft domains
	nbConstraints := spr.GetNbConstraints()
	sizeSystem := uint64(nbConstraints + len(spr.Public)) // len(spr.Public) is for the placeholder constraints
	s.domain0 = fft.NewDomain(sizeSystem)

	// sampling random numbers for blinding the quotient
	if opts.StatisticalZK {
		s.quotientShardsRandomizers[0].SetRandom()
		s.quotientShardsRandomizers[1].SetRandom()
	}

	// h, the quotient polynomial is of degree 3(n+1)+2, so it's in a 3(n+2) dim vector space,
	// the domain is the next power of 2 superior to 3(n+2). 4*domainNum is enough in all cases
	// except when n<6.
	if sizeSystem < 6 {
		s.domain1 = fft.NewDomain(8*sizeSystem, fft.WithoutPrecompute())
	} else {
		s.domain1 = fft.NewDomain(4*sizeSystem, fft.WithoutPrecompute())
	}

	// build trace
	s.trace = plonkbls12381.NewTrace(spr, s.domain0)

	return &s, nil
}

func (s *instance) initBlindingPolynomials() error {
	s.bp[id_Bl] = getRandomPolynomial(order_blinding_L)
	s.bp[id_Br] = getRandomPolynomial(order_blinding_R)
	s.bp[id_Bo] = getRandomPolynomial(order_blinding_O)
	s.bp[id_Bz] = getRandomPolynomial(order_blinding_Z)
	close(s.chbp)
	return nil
}

func (s *instance) initBSB22Commitments() {
	s.commitmentInfo = s.spr.CommitmentInfo.(constraint.PlonkCommitments)
	s.commitmentVal = make([]fr.Element, len(s.commitmentInfo)) // TODO @Tabaie get rid of this
	s.cCommitments = make([]*iop.Polynomial, len(s.commitmentInfo))
	s.proof.Bsb22Commitments = make([]kzg.Digest, len(s.commitmentInfo))

	// override the hint for the commitment constraints
	bsb22ID := solver.GetHintID(fcs.Bsb22CommitmentComputePlaceholder)
	s.opt.SolverOpts = append(s.opt.SolverOpts, solver.OverrideHint(bsb22ID, s.bsb22Hint))
}

// Computing and verifying Bsb22 multi-commits explained in https://hackmd.io/x8KsadW3RRyX7YTCFJIkHg
func (s *instance) bsb22Hint(_ *big.Int, ins, outs []*big.Int) error {
	var err error
	commDepth := int(ins[0].Int64())
	ins = ins[1:]

	res := &s.commitmentVal[commDepth]

	commitmentInfo := s.spr.CommitmentInfo.(constraint.PlonkCommitments)[commDepth]
	committedValues := make([]fr.Element, s.domain0.Cardinality)
	offset := s.spr.GetNbPublicVariables()
	for i := range ins {
		committedValues[offset+commitmentInfo.Committed[i]].SetBigInt(ins[i])
	}
	if _, err = committedValues[offset+commitmentInfo.CommitmentIndex].SetRandom(); err != nil { // Commitment injection constraint has qcp = 0. Safe to use for blinding.
		return err
	}
	if _, err = committedValues[offset+s.spr.GetNbConstraints()-1].SetRandom(); err != nil { // Last constraint has qcp = 0. Safe to use for blinding
		return err
	}
	s.cCommitments[commDepth] = iop.NewPolynomial(&committedValues, iop.Form{Basis: iop.Lagrange, Layout: iop.Regular})
	// if s.proof.Bsb22Commitments[commDepth], err = kzg.Commit(s.cCommitments[commDepth].Coefficients(), s.pk.KzgLagrange); err != nil {
	// 	return err
	// }
	if s.proof.Bsb22Commitments[commDepth], err = commitOnGPUOrCPU(
		s.cCommitments[commDepth].Coefficients(), s.pk, true /* useLagrange */); err != nil {
		return err
	}
	resval := eon.HashCompress(eon.PREFIX_BSB, eon.HashG1(s.proof.Bsb22Commitments[commDepth]))
	res.Set(&resval)
	res.BigInt(outs[0])

	return nil
}

// solveConstraints computes the evaluation of the polynomials L, R, O
// and sets x[id_L], x[id_R], x[id_O] in Lagrange form
func (s *instance) solveConstraints() error {
	start_time := time.Now()
	_solution, err := s.spr.Solve(s.fullWitness, s.opt.SolverOpts...)
	if err != nil {
		return err
	}
	elapsed := time.Since(start_time)
	fmt.Printf("	solveConstraints() || s.spr.Solve() (L, R, O) 耗时: %.6f ms\n", float64(elapsed.Nanoseconds())/1e6)

	start_time = time.Now()
	solution := _solution.(*cs.SparseR1CSSolution)
	evaluationLDomainSmall := []fr.Element(solution.L)
	evaluationRDomainSmall := []fr.Element(solution.R)
	evaluationODomainSmall := []fr.Element(solution.O)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		s.x[id_L] = iop.NewPolynomial(&evaluationLDomainSmall, iop.Form{Basis: iop.Lagrange, Layout: iop.Regular})
		wg.Done()
	}()
	go func() {
		s.x[id_R] = iop.NewPolynomial(&evaluationRDomainSmall, iop.Form{Basis: iop.Lagrange, Layout: iop.Regular})
		wg.Done()
	}()

	s.x[id_O] = iop.NewPolynomial(&evaluationODomainSmall, iop.Form{Basis: iop.Lagrange, Layout: iop.Regular})

	wg.Wait()
	elapsed = time.Since(start_time)
	fmt.Printf("	solveConstraints() || sets x[id_L], x[id_R], x[id_O] 耗时: %.6f ms\n", float64(elapsed.Nanoseconds())/1e6)

	// commit to l, r, o and add blinding factors
	start_time = time.Now()
	if err := s.commitToLRO(); err != nil {
		return err
	}
	elapsed = time.Since(start_time)
	fmt.Printf("	solveConstraints() || commitToLRO() 耗时: %.6f ms\n", float64(elapsed.Nanoseconds())/1e6)

	close(s.chLRO)

	return nil
}

func (s *instance) completeQk() error {
	qk := s.trace.Qk.Clone()
	qkCoeffs := qk.Coefficients()

	wWitness, ok := s.fullWitness.Vector().(fr.Vector)
	if !ok {
		return witness.ErrInvalidWitness
	}

	copy(qkCoeffs, wWitness[:len(s.spr.Public)])

	// wait for solver to be done
	start_time := time.Now()
	select {
	case <-s.ctx.Done():
		return errContextDone
	case <-s.chLRO:
	}
	elapsed := time.Since(start_time)
	fmt.Printf("		completeQk() || wait for solver to be done 耗时: %.6fms\n", float64(elapsed.Nanoseconds())/1e6)

	for i := range s.commitmentInfo {
		qkCoeffs[s.spr.GetNbPublicVariables()+s.commitmentInfo[i].CommitmentIndex] = s.commitmentVal[i]
	}

	s.x[id_Qk] = qk
	close(s.chQk)

	return nil
}

// computeLagrangeOneOnCoset computes 1/n (x**n-1)/(x-1) on coset*ωⁱ
func (s *instance) computeLagrangeOneOnCoset(cosetExpMinusOne fr.Element, index int) fr.Element {
	var res fr.Element
	res.Mul(&cosetExpMinusOne, &s.domain0.CardinalityInv).
		Mul(&res, &s.precomputedDenominators[index])
	return res
}

func (s *instance) commitToLRO() error {
	// wait for blinding polynomials to be initialized or context to be done
	select {
	case <-s.ctx.Done():
		return errContextDone
	case <-s.chbp:
	}

	// g := new(errgroup.Group)

	// g.Go(func() (err error) {
	// 	s.proof.LRO[0], err = s.commitToPolyAndBlinding(s.x[id_L], s.bp[id_Bl])
	// 	return
	// })

	// g.Go(func() (err error) {
	// 	s.proof.LRO[1], err = s.commitToPolyAndBlinding(s.x[id_R], s.bp[id_Br])
	// 	return
	// })

	// g.Go(func() (err error) {
	// 	s.proof.LRO[2], err = s.commitToPolyAndBlinding(s.x[id_O], s.bp[id_Bo])
	// 	return
	// })

	// return g.Wait()

	var err error
	start_time := time.Now()
	if s.proof.LRO[0], err = s.commitToPolyAndBlinding(s.x[id_L], s.bp[id_Bl]); err != nil {
		return err
	}
	elapsed := time.Since(start_time)
	fmt.Printf("		commitToLRO() || commitToPolyAndBlinding(s.x[id_L], s.bp[id_Bl]) LRO[0] 耗时: %.6fms\n", float64(elapsed.Nanoseconds())/1e6)

	start_time = time.Now()
	if s.proof.LRO[1], err = s.commitToPolyAndBlinding(s.x[id_R], s.bp[id_Br]); err != nil {
		return err
	}
	elapsed = time.Since(start_time)
	fmt.Printf("		commitToLRO() || commitToPolyAndBlinding(s.x[id_R], s.bp[id_Br]) LRO[1] 耗时: %.6fms\n", float64(elapsed.Nanoseconds())/1e6)

	start_time = time.Now()
	if s.proof.LRO[2], err = s.commitToPolyAndBlinding(s.x[id_O], s.bp[id_Bo]); err != nil {
		return err
	}
	elapsed = time.Since(start_time)
	fmt.Printf("		commitToLRO() || commitToPolyAndBlinding(s.x[id_O], s.bp[id_Bo]) LRO[2] 耗时: %.6fms\n", float64(elapsed.Nanoseconds())/1e6)

	return nil
}

// func (s *instance) commitToLRO() error {
// 	// 等 blinding 好
// 	select {
// 	case <-s.ctx.Done():
// 		return errContextDone
// 	case <-s.chbp:
// 	}

// 	// 优先走 GPU batch 路径
// 	if HasIcicle && s.pk != nil && s.pk.deviceInfo != nil {
// 		coeffL := s.x[id_L].Coefficients()
// 		coeffR := s.x[id_R].Coefficients()
// 		coeffO := s.x[id_O].Coefficients()

// 		var (
// 			digs []kzg.Digest
// 			st   icicle_runtime.EIcicleError
// 		)
// 		done := make(chan struct{})

// 		start := time.Now()
// 		t0 := time.Now()
// 		icicle_runtime.RunOnDevice(&s.pk.deviceInfo.Device, func(args ...any) {
// 			defer close(done)
// 			base := s.pk.deviceInfo.G1Device.G1Lagrange.RangeTo(len(coeffL), false)
// 			digs, st = kzg_bls12_381.OnDeviceCommitBatchLRO(
// 				[][]fr.Element{coeffL, coeffR, coeffO},
// 				base,
// 			)
// 		})
// 		<-done
// 		t1 := time.Since(t0)

// 		if st == icicle_runtime.Success {
// 			// 把 batch 出来的三个 digest 先当作“无 blinding 的 commit”
// 			cL := curve.G1Affine(digs[0])
// 			cR := curve.G1Affine(digs[1])
// 			cO := curve.G1Affine(digs[2])

// 			n := int(s.domain0.Cardinality)

// 			t2Start := time.Now()
// 			// 然后给每个加上 blinding contribution
// 			cbL, err := commitBlindingFactorGPUOrCPU(n, s.bp[id_Bl], s.pk)
// 			if err != nil {
// 				return err
// 			}
// 			cbR, err := commitBlindingFactorGPUOrCPU(n, s.bp[id_Br], s.pk)
// 			if err != nil {
// 				return err
// 			}
// 			cbO, err := commitBlindingFactorGPUOrCPU(n, s.bp[id_Bo], s.pk)
// 			if err != nil {
// 				return err
// 			}
// 			t2 := time.Since(t2Start)
// 			total := time.Since(start)
// 			log.Printf("[TIMING] commitToLRO: total=%v, bigMSM=%v, blinding=%v", total, t1, t2)

// 			cL.Add(&cL, &cbL)
// 			cR.Add(&cR, &cbR)
// 			cO.Add(&cO, &cbO)

// 			s.proof.LRO[0] = cL
// 			s.proof.LRO[1] = cR
// 			s.proof.LRO[2] = cO
// 			return nil
// 		}

// 		// GPU 失败就退回 CPU 串行老逻辑
// 		log.Printf("[GPU failed -> CPU] commitToLRO batch: %s", st.AsString())
// 	}
// 	return nil
// }

// deriveGammaAndBeta (copy constraint)
func (s *instance) deriveGammaAndBeta() error {
	wWitness, ok := s.fullWitness.Vector().(fr.Vector)
	if !ok {
		return witness.ErrInvalidWitness
	}

	if err := Dev_bindPublicData(s.fs, eon.CID_GAMMA, s.pk.Vk, wWitness[:len(s.spr.Public)]); err != nil {
		return err
	}

	// wait for LRO to be committed
	start_time := time.Now()
	select {
	case <-s.ctx.Done():
		return errContextDone
	case <-s.chLRO:
	}
	elapsed := time.Since(start_time)
	fmt.Printf("		deriveGammaAndBeta() || wait for LRO to be committed 耗时: %.6fms\n", float64(elapsed.Nanoseconds())/1e6)

	start_time = time.Now()
	if err := s.fs.Bind(eon.CID_GAMMA, eon.HashG1(s.proof.LRO[0])); err != nil {
		return err
	}
	if err := s.fs.Bind(eon.CID_GAMMA, eon.HashG1(s.proof.LRO[1])); err != nil {
		return err
	}
	if err := s.fs.Bind(eon.CID_GAMMA, eon.HashG1(s.proof.LRO[2])); err != nil {
		return err
	}
	if err := s.fs.Bind(eon.CID_GAMMA, wWitness[:len(s.spr.Public)]...); err != nil {
		return err
	}

	gamma, err := Dev_deriveRandomness(s.fs, eon.CID_GAMMA)
	if err != nil {
		return err
	}

	bbeta, err := s.fs.ComputeChallenge(eon.CID_BETA)
	if err != nil {
		return err
	}
	s.gamma = gamma
	s.beta = bbeta

	close(s.chGammaBeta)

	return nil
}

// commitToPolyAndBlinding computes the KZG commitment of a polynomial p
// in Lagrange form (large degree)
// and add the contribution of a blinding polynomial b (small degree)
// /!\ The polynomial p is supposed to be in Lagrange form.
func (s *instance) commitToPolyAndBlinding(p, b *iop.Polynomial) (commit curve.G1Affine, err error) {

	// if HasIcicle && s.pk != nil && s.pk.deviceInfo != nil {
	// 	var dig kzg.Digest
	// 	var st icicle_runtime.EIcicleError

	// 	done := make(chan struct{})
	// 	icicle_runtime.RunOnDevice(&s.pk.deviceInfo.Device, func(args ...any) {
	// 		defer close(done)
	// 		dig, st = kzg_bls12_381.OnDeviceCommit(p.Coefficients(), s.pk.deviceInfo.G1Device.G1Lagrange)
	// 	})
	// 	<-done

	// 	if st == icicle_runtime.Success {
	// 		log.Printf("[GPU success] kzg.Commit")
	// 		commit = curve.G1Affine(dig)
	// 	} else {
	// 		// GPU 失败 → CPU 回退
	// 		log.Printf("[GPU failed -> CPU] kzg.Commit")
	// 		commit, err = kzg.Commit(p.Coefficients(), s.pk.KzgLagrange)
	// 		if err != nil {
	// 			return curve.G1Affine{}, err
	// 		}
	// 	}
	// } else {
	// 	// 无 GPU → 直接 CPU
	// 	log.Printf("[CPU] kzg.Commit")
	// 	commit, err = kzg.Commit(p.Coefficients(), s.pk.KzgLagrange)
	// 	if err != nil {
	// 		return curve.G1Affine{}, err
	// 	}
	// }
	start_time := time.Now()
	commit, err = commitOnGPUOrCPU(p.Coefficients(), s.pk, true)
	elasped := time.Since(start_time)
	fmt.Printf("			commitToPolyAndBlinding() -> commit to p 耗时: %.6fms\n", float64(elasped.Nanoseconds())/1e6)

	// commit, err = kzg.Commit(p.Coefficients(), s.pk.KzgLagrange)

	// we add in the blinding contribution
	start_time = time.Now()
	n := int(s.domain0.Cardinality)
	// cb := commitBlindingFactor(n, b, s.pk.Kzg)
	cb, err2 := commitBlindingFactorGPUOrCPU(n, b, s.pk)
	elasped = time.Since(start_time)
	fmt.Printf("			commitToPolyAndBlinding() -> commit to b 耗时: %.6fms\n", float64(elasped.Nanoseconds())/1e6)
	if err2 != nil {
		return curve.G1Affine{}, err2
	}

	start_time = time.Now()
	commit.Add(&commit, &cb)
	elasped = time.Since(start_time)
	fmt.Printf("			commitToPolyAndBlinding() -> commit_p + commit_b 耗时: %.6fms\n", float64(elasped.Nanoseconds())/1e6)

	return
}

func (s *instance) deriveAlpha() (err error) {
	alphaDeps := make([]*curve.G1Affine, len(s.proof.Bsb22Commitments)+1)
	for i := range s.proof.Bsb22Commitments {
		alphaDeps[i] = &s.proof.Bsb22Commitments[i]
	}
	alphaDeps[len(alphaDeps)-1] = &s.proof.Z
	s.alpha, err = Dev_deriveRandomness(s.fs, eon.CID_ALPHA, alphaDeps...)
	return err
}

func (s *instance) deriveZeta() (err error) {
	s.zeta, err = Dev_deriveRandomness(s.fs, eon.CID_ZETA, &s.proof.H[0], &s.proof.H[1], &s.proof.H[2])
	return
}

// computeQuotient computes H
func (s *instance) computeQuotient() (err error) {
	start_time := time.Now()

	s.x[id_Ql] = s.trace.Ql
	s.x[id_Qr] = s.trace.Qr
	s.x[id_Qm] = s.trace.Qm
	s.x[id_Qo] = s.trace.Qo
	s.x[id_S1] = s.trace.S1
	s.x[id_S2] = s.trace.S2
	s.x[id_S3] = s.trace.S3

	for i := 0; i < len(s.commitmentInfo); i++ {
		s.x[id_Qci+2*i] = s.trace.Qcp[i]
	}

	n := s.domain0.Cardinality
	lone := make([]fr.Element, n)
	lone[0].SetOne()

	// wait for solver to be done
	start_wait_for_solver_done := time.Now()
	select {
	case <-s.ctx.Done():
		return errContextDone
	case <-s.chLRO:
	}
	elapsed := time.Since(start_wait_for_solver_done)
	fmt.Printf("		computeQuotient() || wait for solver to be done 耗时: %.6fms\n", float64(elapsed.Nanoseconds())/1e6)

	for i := 0; i < len(s.commitmentInfo); i++ {
		s.x[id_Qci+2*i+1] = s.cCommitments[i]
	}

	// wait for Z to be committed or context done
	start_wait_for_z_committed := time.Now()
	select {
	case <-s.ctx.Done():
		return errContextDone
	case <-s.chZ:
	}
	elapsed = time.Since(start_wait_for_z_committed)
	fmt.Printf("		computeQuotient() || wait for Z to be committed 耗时: %.6fms\n", float64(elapsed.Nanoseconds())/1e6)

	// derive alpha
	if err = s.deriveAlpha(); err != nil {
		return err
	}

	// TODO complete waste of memory find another way to do that
	identity := make([]fr.Element, n)
	identity[1].Set(&s.beta)

	s.x[id_ZS] = s.x[id_Z].ShallowClone().Shift(1)
	elasped := time.Since(start_time)
	fmt.Printf("	computeQuotient() || prepare to compute & derive alpha): %.6f ms\n", float64(elasped.Nanoseconds())/1e6)

	start_time = time.Now()
	numerator, err := s.computeNumerator()
	if err != nil {
		return err
	}
	elasped = time.Since(start_time)
	fmt.Printf("	computeQuotient() || computeNumerator() 耗时: %.6f ms\n", float64(elasped.Nanoseconds())/1e6)

	start_time = time.Now()
	s.h, err = divideByZH(numerator, [2]*fft.Domain{s.domain0, s.domain1})
	if err != nil {
		return err
	}
	elasped = time.Since(start_time)
	fmt.Printf("	computeQuotient() || divideByZH() 耗时: %.6f ms\n", float64(elasped.Nanoseconds())/1e6)

	start_time = time.Now()
	// commit to h
	// if err := commitToQuotient(s.h1(), s.h2(), s.h3(), s.proof, s.pk.Kzg); err != nil {
	if err := commitToQuotient(s.h1(), s.h2(), s.h3(), s.proof, s.pk); err != nil {
		return err
	}
	elasped = time.Since(start_time)
	fmt.Printf("	computeQuotient() || commitToQuotient() 耗时: %.6f ms\n", float64(elasped.Nanoseconds())/1e6)

	start_time = time.Now()
	if err := s.deriveZeta(); err != nil {
		return err
	}

	// wait for clean up tasks to be done
	select {
	case <-s.ctx.Done():
		return errContextDone
	case <-s.chRestoreLRO:
	}

	close(s.chH)
	elasped = time.Since(start_time)
	fmt.Printf("	computeQuotient() || deriveZeta() & clean up 耗时: %.6f ms\n", float64(elasped.Nanoseconds())/1e6)

	return nil
}

func (s *instance) buildRatioCopyConstraint() (err error) {
	// wait for gamma and beta to be derived (or ctx.Done())
	start_time := time.Now()
	select {
	case <-s.ctx.Done():
		return errContextDone
	case <-s.chGammaBeta:
	}
	elapsed := time.Since(start_time)
	fmt.Printf("		buildRatioCopyConstraint() || wait for gamma and beta to be derived 耗时: %.6fms\n", float64(elapsed.Nanoseconds())/1e6)

	start_time = time.Now()
	// TODO @gbotrel having iop.BuildRatioCopyConstraint return something
	// with capacity = len() + 4 would avoid extra alloc / copy during openZ
	s.x[id_Z], err = iop.BuildRatioCopyConstraint(
		[]*iop.Polynomial{
			s.x[id_L],
			s.x[id_R],
			s.x[id_O],
		},
		s.trace.S,
		s.beta,
		s.gamma,
		iop.Form{Basis: iop.Lagrange, Layout: iop.Regular},
		s.domain0,
	)
	if err != nil {
		return err
	}
	elapsed = time.Since(start_time)
	fmt.Printf("		buildRatioCopyConstraint() || iop.BuildRatioCopyConstraint 耗时: %.6fms\n", float64(elapsed.Nanoseconds())/1e6)

	// commit to the blinded version of z
	start_time = time.Now()
	s.proof.Z, err = s.commitToPolyAndBlinding(s.x[id_Z], s.bp[id_Bz])
	elapsed = time.Since(start_time)
	fmt.Printf("		buildRatioCopyConstraint() || commitToPolyAndBlinding(s.x[id_Z], s.bp[id_Bz]) 耗时: %.6fms\n", float64(elapsed.Nanoseconds())/1e6)

	close(s.chZ)

	return
}

// open Z (blinded) at ωζ
func (s *instance) openZ() (err error) {
	// wait for H to be committed and zeta to be derived (or ctx.Done())
	start_time := time.Now()
	select {
	case <-s.ctx.Done():
		return errContextDone
	case <-s.chH:
	}
	elapsed := time.Since(start_time)
	fmt.Printf("		openZ() || wait for H to be committed and zeta to be derived 耗时: %.6fms\n", float64(elapsed.Nanoseconds())/1e6)

	start_time = time.Now()
	var zetaShifted fr.Element
	zetaShifted.Mul(&s.zeta, &s.pk.Vk.Generator)
	s.blindedZ = getBlindedCoefficients(s.x[id_Z], s.bp[id_Bz])
	elapsed = time.Since(start_time)
	fmt.Printf("		openZ() || getBlindedCoefficients 耗时: %.6fms\n", float64(elapsed.Nanoseconds())/1e6)

	// open z at zeta
	// s.proof.ZShiftedOpening, err = kzg.Open(s.blindedZ, zetaShifted, s.pk.Kzg)
	start_time = time.Now()
	s.proof.ZShiftedOpening, err = OpenOnGPUOrCPU(s.blindedZ, zetaShifted, s.pk)
	elapsed = time.Since(start_time)
	fmt.Printf("		openZ() || Open 耗时: %.6fms\n", float64(elapsed.Nanoseconds())/1e6)
	if err != nil {
		return err
	}
	close(s.chZOpening)
	return nil
}

func (s *instance) h1() []fr.Element {
	var h1 []fr.Element
	if !s.opt.StatisticalZK {
		h1 = s.h.Coefficients()[:s.domain0.Cardinality+2]
	} else {
		h1 = make([]fr.Element, s.domain0.Cardinality+3)
		copy(h1, s.h.Coefficients()[:s.domain0.Cardinality+2])
		h1[s.domain0.Cardinality+2].Set(&s.quotientShardsRandomizers[0])
	}
	return h1
}

func (s *instance) h2() []fr.Element {
	var h2 []fr.Element
	if !s.opt.StatisticalZK {
		h2 = s.h.Coefficients()[s.domain0.Cardinality+2 : 2*(s.domain0.Cardinality+2)]
	} else {
		h2 = make([]fr.Element, s.domain0.Cardinality+3)
		copy(h2, s.h.Coefficients()[s.domain0.Cardinality+2:2*(s.domain0.Cardinality+2)])
		h2[0].Sub(&h2[0], &s.quotientShardsRandomizers[0])
		h2[s.domain0.Cardinality+2].Set(&s.quotientShardsRandomizers[1])
	}
	return h2
}

func (s *instance) h3() []fr.Element {
	var h3 []fr.Element
	if !s.opt.StatisticalZK {
		h3 = s.h.Coefficients()[2*(s.domain0.Cardinality+2) : 3*(s.domain0.Cardinality+2)]
	} else {
		h3 = make([]fr.Element, s.domain0.Cardinality+2)
		copy(h3, s.h.Coefficients()[2*(s.domain0.Cardinality+2):3*(s.domain0.Cardinality+2)])
		h3[0].Sub(&h3[0], &s.quotientShardsRandomizers[1])
	}
	return h3
}

func (s *instance) computeLinearizedPolynomial() error {
	start_time := time.Now()
	// wait for H to be committed and zeta to be derived (or ctx.Done())
	select {
	case <-s.ctx.Done():
		return errContextDone
	case <-s.chH:
	}
	elapsed := time.Since(start_time)
	fmt.Printf("		computeLinearizedPolynomial() || wait for H to be committed and zeta to be derived 耗时: %.6fms\n", float64(elapsed.Nanoseconds())/1e6)

	start_time = time.Now()
	qcpzeta := make([]fr.Element, len(s.commitmentInfo))
	var blzeta, brzeta, bozeta fr.Element
	var wg sync.WaitGroup
	wg.Add(3 + len(s.commitmentInfo))

	for i := 0; i < len(s.commitmentInfo); i++ {
		go func(i int) {
			qcpzeta[i] = s.trace.Qcp[i].Evaluate(s.zeta)
			wg.Done()
		}(i)
	}

	go func() {
		blzeta = evaluateBlinded(s.x[id_L], s.bp[id_Bl], s.zeta)
		wg.Done()
	}()

	go func() {
		brzeta = evaluateBlinded(s.x[id_R], s.bp[id_Br], s.zeta)
		wg.Done()
	}()

	go func() {
		bozeta = evaluateBlinded(s.x[id_O], s.bp[id_Bo], s.zeta)
		wg.Done()
	}()

	// wait for Z to be opened at zeta (or ctx.Done())
	select {
	case <-s.ctx.Done():
		return errContextDone
	case <-s.chZOpening:
	}
	bzuzeta := s.proof.ZShiftedOpening.ClaimedValue

	wg.Wait()

	s.linearizedPolynomial = s.innerComputeLinearizedPoly(
		blzeta,
		brzeta,
		bozeta,
		s.alpha,
		s.beta,
		s.gamma,
		s.zeta,
		bzuzeta,
		qcpzeta,
		s.blindedZ,
		coefficients(s.cCommitments),
		s.pk,
	)
	elapsed = time.Since(start_time)
	fmt.Printf("		computeLinearizedPolynomial() || innerComputeLinearizedPoly 耗时: %.6fms\n", float64(elapsed.Nanoseconds())/1e6)

	var err error
	// s.linearizedPolynomialDigest, err = kzg.Commit(s.linearizedPolynomial, s.pk.Kzg, runtime.NumCPU()*2)
	start_time = time.Now()
	s.linearizedPolynomialDigest, err = commitOnGPUOrCPU(s.linearizedPolynomial, s.pk, false /* monomial */)
	elapsed = time.Since(start_time)
	fmt.Printf("		computeLinearizedPolynomial() || kzg.Commit 耗时: %.6f ms\n", float64(elapsed.Nanoseconds())/1e6)

	if err != nil {
		return err
	}
	close(s.chLinearizedPolynomial)
	return nil
}

func (s *instance) batchOpening() error {

	// wait for linearizedPolynomial to be computed (or ctx.Done())
	start_time := time.Now()
	select {
	case <-s.ctx.Done():
		return errContextDone
	case <-s.chLinearizedPolynomial:
	}
	elapsed := time.Since(start_time)
	fmt.Printf("		batchOpening() || wait for linearizedPolynomial 耗时: %.6fms\n", float64(elapsed.Nanoseconds())/1e6)

	start_time = time.Now()
	polysQcp := coefficients(s.trace.Qcp)
	polysToOpen := make([][]fr.Element, 6+len(polysQcp))
	copy(polysToOpen[6:], polysQcp)

	polysToOpen[0] = s.linearizedPolynomial
	polysToOpen[1] = getBlindedCoefficients(s.x[id_L], s.bp[id_Bl])
	polysToOpen[2] = getBlindedCoefficients(s.x[id_R], s.bp[id_Br])
	polysToOpen[3] = getBlindedCoefficients(s.x[id_O], s.bp[id_Bo])
	polysToOpen[4] = s.trace.S1.Coefficients()
	polysToOpen[5] = s.trace.S2.Coefficients()

	digestsToOpen := make([]curve.G1Affine, len(s.pk.Vk.Qcp)+6)
	copy(digestsToOpen[6:], s.pk.Vk.Qcp)

	digestsToOpen[0] = s.linearizedPolynomialDigest
	digestsToOpen[1] = s.proof.LRO[0]
	digestsToOpen[2] = s.proof.LRO[1]
	digestsToOpen[3] = s.proof.LRO[2]
	digestsToOpen[4] = s.pk.Vk.S[0]
	digestsToOpen[5] = s.pk.Vk.S[1]
	elapsed = time.Since(start_time)
	fmt.Printf("		batchOpening() || prepare polysToOpen and digestsToOpen 耗时: %.6fms\n", float64(elapsed.Nanoseconds())/1e6)

	start_time = time.Now()
	var err error
	s.proof.BatchedProof, err = BatchOpenSinglePoint(
		polysToOpen,
		digestsToOpen,
		s.zeta,
		// s.pk.Kzg,
		s.pk,
		s.proof.ZShiftedOpening.ClaimedValue,
	)
	elapsed = time.Since(start_time)
	fmt.Printf("		batchOpening() || BatchOpenSinglePoint 耗时: %.6fms\n", float64(elapsed.Nanoseconds())/1e6)
	return err
}

// 函数的目标是：在大域上算出num的点值，存在(cres)中，
//
//	后续：再逐点除Z_H得t的点值
//		 再对长度为∣domain1∣=ρn的数组做INTT，得到t的系数形式
//		 最后对每n个系数切出{ti}， 再分别KZG commit
//
// evaluate the full set of constraints, all polynomials in x are back in
// canonical regular form at the end
func (s *instance) computeNumerator() (*iop.Polynomial, error) {
	// init vectors that are used multiple times throughout the computation

	// —————————————————————————————————————————————————————————————————————————— 准备小域H的幂表[1,𝜔,𝜔^2,…,𝜔^𝑛−1], 对于每一个coset来说，第i个点小域坐标(块内相位)都是𝜔^i，实际上evaluation的point是 coset_j * 𝜔^i
	n := s.domain0.Cardinality
	twiddles0 := make([]fr.Element, n)
	if n == 1 {
		// edge case
		twiddles0[0].SetOne()
	} else {
		twiddles, err := s.domain0.Twiddles()
		if err != nil {
			return nil, err
		}
		copy(twiddles0, twiddles[0])
		w := twiddles0[1]
		for i := len(twiddles[0]); i < len(twiddles0); i++ {
			twiddles0[i].Mul(&twiddles0[i-1], &w)
		}
	}
	// —————————————————————————————————————————————————————————————————————————— 等待 Qk 准备好
	// wait for chQk to be closed (or ctx.Done())
	select {
	case <-s.ctx.Done():
		return nil, errContextDone
	case <-s.chQk:
	}

	// —————————————————————————————————————————————————————————————————————————— 算门约束 gate constraint Ql​L+Qr​R+Qm​LR+Qo​O+Qk​+∑Qci​Qci+1​ 在大域上的evaluation点值，也就是在X_{i,j} = coset_j * 𝜔^i 上的值
	nbBsbGates := len(s.proof.Bsb22Commitments)

	gateConstraint := func(u ...fr.Element) fr.Element {

		var ic, tmp fr.Element

		ic.Mul(&u[id_Ql], &u[id_L])
		tmp.Mul(&u[id_Qr], &u[id_R])
		ic.Add(&ic, &tmp)
		tmp.Mul(&u[id_Qm], &u[id_L]).Mul(&tmp, &u[id_R])
		ic.Add(&ic, &tmp)
		tmp.Mul(&u[id_Qo], &u[id_O])
		ic.Add(&ic, &tmp).Add(&ic, &u[id_Qk])
		for i := 0; i < nbBsbGates; i++ {
			tmp.Mul(&u[id_Qci+2*i], &u[id_Qci+2*i+1])
			ic.Add(&ic, &tmp)
		}

		return ic
	}

	// —————————————————————————————————————————————————————————————————————————— 生成g和g^2, 用于在 PLONK 置换约束里，分母那边是(L+γ+β⋅x)(R+γ+β⋅gx)(O+γ+β⋅g^2x)
	var cs, css fr.Element
	cs.Set(&s.domain1.FrMultiplicativeGen)
	css.Square(&cs)

	// stores the current coset shifter
	var coset fr.Element
	coset.SetOne()

	// cosetExponentiatedToNMinusOne stores <coset>^n-1
	var cosetExponentiatedToNMinusOne, one fr.Element
	one.SetOne()
	bn := big.NewInt(int64(n))

	// —————————————————————————————————————————————————————————————————————————— 标准的 Grand Product 约束：(L+γ+βS1​)(R+γ+βS2​)(O+γ+βS3​)Z(ωX)−(L+γ+βX)(R+γ+βgX)(O+γ+βg2X)Z(X)
	orderingConstraint := func(index int, u ...fr.Element) fr.Element {

		gamma := s.gamma

		// ordering constraint
		var a, b, c, r, l, id fr.Element

		// evaluation of ID at coset*ωⁱ where i:=index
		id.Mul(&twiddles0[index], &coset).Mul(&id, &s.beta)

		// 右侧 (分母) 的三项：L + γ + id, R + γ + id*g, O + γ + id*g^2
		a.Add(&gamma, &u[id_L]).Add(&a, &id)
		b.Mul(&id, &cs).Add(&b, &u[id_R]).Add(&b, &gamma)
		c.Mul(&id, &css).Add(&c, &u[id_O]).Add(&c, &gamma)
		r.Mul(&a, &b).Mul(&r, &c).Mul(&r, &u[id_Z])

		// 左侧 (分子) 的三项：L + γ + βS1, R + γ + βS2, O + γ + βS3
		a.Add(&u[id_S1], &u[id_L]).Add(&a, &gamma)
		b.Add(&u[id_S2], &u[id_R]).Add(&b, &gamma)
		c.Add(&u[id_S3], &u[id_O]).Add(&c, &gamma)
		l.Mul(&a, &b).Mul(&l, &c).Mul(&l, &u[id_ZS])

		l.Sub(&l, &r)

		return l
	}

	// —————————————————————————————————————————————————————————————————————————— 算(1−Z(X))⋅L1​(X), 在大域上的evaluation点值，也就是在X_{i,j} = coset_j * 𝜔^i 上的值
	localConstraint := func(index int, u ...fr.Element) fr.Element {
		// local constraint
		var res, lone fr.Element
		// 这一步给出 L1(X) = 1/n * (X^n - 1)/(X - 1) on coset*ωⁱ
		lone = s.computeLagrangeOneOnCoset(cosetExponentiatedToNMinusOne, index)
		res.SetOne()
		res.Sub(&u[id_Z], &res).Mul(&res, &lone)

		return res
	}

	// —————————————————————————————————————————————————————————————————————————— 算 行数 = ρ（coset 块），第一个coset偏移量（shifters[0]）为s，之后的步长（shifters[i>=1]）都为w，真实评估点为Xi,j​=(s⋅wi)⋅ωj,j=0,…,n−1,
	rho := int(s.domain1.Cardinality / n)
	shifters := make([]fr.Element, rho)
	// 选一个不在小域 H里的乘法生成元 s，作为首块的 coset 偏移
	shifters[0].Set(&s.domain1.FrMultiplicativeGen)
	for i := 1; i < rho; i++ {
		shifters[i].Set(&s.domain1.Generator)
	}

	// —————————————————————————————————————————————————————————————————————————— cosetTable本质上是在算一个长度为n的[1,s,s2,…,s^n−1], 用于把系数按幂次乘上 𝑠^𝑘, 然后在domain0做FFT就可以得到在coset s·H上的n个点值
	// cosetTable, err := s.domain0.CosetTable()
	// if err != nil {
	// 	return nil, err
	// }

	// —————————————————————————————————————————————————————————————————————————— cres存整个大域的点值，buf存当前n个点的中间结果
	// init the result polynomial & buffer
	cres := make([]fr.Element, s.domain1.Cardinality)
	buf := make([]fr.Element, n)
	var wgBuf sync.WaitGroup

	// —————————————————————————————————————————————————————————————————————————— 整合三类约束为“分子”的点值（allConstraints）
	allConstraints := func(index int, u ...fr.Element) fr.Element {

		// scale S1, S2, S3 by β
		// ① S1,S2,S3 ← β·S*
		u[id_S1].Mul(&u[id_S1], &s.beta)
		u[id_S2].Mul(&u[id_S2], &s.beta)
		u[id_S3].Mul(&u[id_S3], &s.beta)

		// blind L, R, O, Z, ZS
		// ② blind: L,R,O,Z,ZS ← + b(ω^index) ；ZS 用 (index+1)%n
		var y fr.Element
		y = s.bp[id_Bl].Evaluate(twiddles0[index])
		u[id_L].Add(&u[id_L], &y)
		y = s.bp[id_Br].Evaluate(twiddles0[index])
		u[id_R].Add(&u[id_R], &y)
		y = s.bp[id_Bo].Evaluate(twiddles0[index])
		u[id_O].Add(&u[id_O], &y)
		y = s.bp[id_Bz].Evaluate(twiddles0[index])
		u[id_Z].Add(&u[id_Z], &y)

		// ZS is shifted by 1; need to get correct twiddle
		y = s.bp[id_Bz].Evaluate(twiddles0[(index+1)%int(n)])
		u[id_ZS].Add(&u[id_ZS], &y)

		// ③ a + α b + α^2 c  （写成 ((c*α + b)*α + a) 避免多次 temp）
		a := gateConstraint(u...)
		b := orderingConstraint(index, u...)
		c := localConstraint(index, u...)
		c.Mul(&c, &s.alpha).Add(&c, &b).Mul(&c, &s.alpha).Add(&c, &a)
		return c
	}

	// ———————————————————————————————————————————————————————————————————————————— 准备缩放向量，系数 × 缩放向量 + 长度 n 的 FFT = 在当前 coset 上评值

	// // for the first iteration, the scalingVector is the coset table
	// scalingVector := cosetTable
	// scalingVectorRev := make([]fr.Element, len(cosetTable))
	// copy(scalingVectorRev, cosetTable)
	// fft.BitReverse(scalingVectorRev)

	// pre-computed to compute the bit reverse index
	// of the result polynomial
	m := uint64(s.domain1.Cardinality)
	mm := uint64(64 - bits.TrailingZeros64(m))

	// ========= 仅在 computeNumerator 内部：把参与的多项式系数上传到 device =========
	useGPU := HasIcicle && s.pk != nil && s.pk.deviceInfo != nil
	var devX []icicle_core.DeviceSlice
	var uploadedIdx []int
	var poly2idx map[*iop.Polynomial]int

	if useGPU {
		devX = make([]icicle_core.DeviceSlice, len(s.x))
		uploadedIdx = make([]int, 0, len(s.x))
		poly2idx = make(map[*iop.Polynomial]int, len(s.x))

		var upErr error
		doneUpload := make(chan struct{})

		icicle_runtime.RunOnDevice(&s.pk.deviceInfo.Device, func(args ...any) {
			defer close(doneUpload)
			for i := 0; i < len(s.x); i++ {
				if i == id_ZS || s.x[i] == nil {
					continue
				}

				// 上传到同一张卡
				host := icicle_core.HostSliceFromElements(s.x[i].Coefficients())
				host.CopyToDevice(&devX[i], true)

				// device 侧统一规范为 Canonical（后续每轮：系数×缩放→NTT）
				if s.x[i].Basis != iop.Canonical {
					if st := kzg_bls12_381.INttOnDevice(devX[i]); st != icicle_runtime.Success {
						upErr = fmt.Errorf("INttOnDevice poly[%d]: %s", i, st.AsString())
						return
					}
				}
				uploadedIdx = append(uploadedIdx, i)
				poly2idx[s.x[i]] = i
			}
		})
		<-doneUpload
		if upErr != nil {
			// 上传失败 → 放弃 GPU 路径
			useGPU = false
			// 清理已分配的 DeviceSlice
			icicle_runtime.RunOnDevice(&s.pk.deviceInfo.Device, func(args ...any) {
				for _, idx := range uploadedIdx {
					devX[idx].Free()

				}
			})
		}
	}

	// ———————————————————————————————————————————————————————————————————————————— 分配两条长度 n 的数组，稍后装 1/(coset⋅ω^j−1)
	s.precomputedDenominators = make([]fr.Element, s.domain0.Cardinality)
	bufBatchInvert := make([]fr.Element, s.domain0.Cardinality)

	// ———————————————————————————————————————————————————————————————————————————— 对每一个 coset 块，做以下操作：定 coset → 备 L₁ 分母 → 调整 blind（加常数&相位）→（i=1 起换缩放表）→ 系数×缩放+小 FFT → 逐点评约束 → 写入大域 → 撤常数保相位。
	for i := 0; i < rho; i++ {

		// 把“当前块”的 coset 变成 s·w^i；同时算出 (s⋅w^i)ⁿ−1
		coset.Mul(&coset, &shifters[i]) // i=0: s; i=1: s·w; i=2: s·w²; ...
		cosetExponentiatedToNMinusOne.Exp(coset, bn).
			Sub(&cosetExponentiatedToNMinusOne, &one)

		// 为本块一次性算好 1/(coset⋅ω^j−1)
		for j := 0; j < int(s.domain0.Cardinality); j++ {
			s.precomputedDenominators[j].
				Mul(&coset, &twiddles0[j]).
				Sub(&s.precomputedDenominators[j], &one)
		}
		batchInvert(s.precomputedDenominators, bufBatchInvert)

		// 调整 blinding 多项式的系数（适配本块）,把每个 blind 多项式的“第 j 项系数”乘上 (coset^n−1)⋅(shifters[i])^j
		// bl <- bl *( (s*ωⁱ)ⁿ-1 )s
		for _, q := range s.bp {
			cq := q.Coefficients()
			acc := cosetExponentiatedToNMinusOne
			for j := 0; j < len(cq); j++ {
				cq[j].Mul(&cq[j], &acc)
				acc.Mul(&acc, &shifters[i])
			}
		}
		// 从第 2 块开始换缩放向量, 仅在i=1时把缩放向量从“cosetTable(s)”换成“w^j 幂表”
		// 选本轮缩放向量（Device & Host 各一份；Regular/BitReverse 两个版本）
		// var wDevRegular, wDevRev icicle_core.DeviceSlice
		var wDevReg, wDevRev icicle_core.DeviceSlice
		var sk scalingKind
		if i == 0 {
			// 第 0 块：coset 表
			wDevReg = s.pk.deviceInfo.CosetTable
			wDevRev = s.pk.deviceInfo.CosetTableRev
			sk = scaleCoset

		} else {
			// 其余块：大域 w^j 表
			wDevReg = s.pk.deviceInfo.BigTwiddlesN
			wDevRev = s.pk.deviceInfo.BigTwiddlesNRev
			sk = scaleBig
		}

		// 把所有参与的多项式转换成“本块 coset 的 n 个点值”
		// we do **a lot** of FFT here, but on the small domain.
		// note that for all the polynomials in the proving key
		// (Ql, Qr, Qm, Qo, S1, S2, S3, Qcp, Qc) and ID, LOne
		// we could pre-compute these rho*2 FFTs and store them
		// at the cost of a huge memory footprint.
		batchApply(s.x, func(p *iop.Polynomial) {
			if p == nil {
				return
			}
			// 根据 p.Layout 选择 Regular/BitReverse 的 DeviceSlice；
			// 同时把 Host 的 Regular/Rev（仅在 CPU 回退时使用）也传入。
			if useGPU {
				if idx, ok := poly2idx[p]; ok {
					_ = s.toCosetLagrangeOnGPUorCPU_DEV(p, wDevReg, wDevRev, sk, &devX[idx])
					return
				}
			}
			// GPU 不可用或该 poly 未上传 → CPU 回退
			_ = s.toCosetLagrangeOnGPUorCPU_DEV(p, wDevReg, wDevRev, sk, nil)
		})

		wgBuf.Wait()

		// 计算gateConstraint(u) + α·orderingConstraint(index,u) + α²·localConstraint(index,u)，把分子在这 n 个点的值写入 buf[j]
		if _, err := iop.Evaluate(
			allConstraints,
			buf,
			iop.Form{Basis: iop.Lagrange, Layout: iop.Regular},
			s.x...,
		); err != nil {
			return nil, err
		}
		wgBuf.Add(1)
		go func(i int) {
			for j := 0; j < int(n); j++ {
				// we build the polynomial in bit reverse order
				cres[bits.Reverse64(uint64(rho*j+i))>>mm] = buf[j]
			}
			wgBuf.Done()
		}(i)

		// 把本块开始时为 blind 系数乘过的 (coset^n - 1) 乘回逆元撤掉
		cosetExponentiatedToNMinusOne.
			Inverse(&cosetExponentiatedToNMinusOne)
		// bl <- bl *( (s*ωⁱ)ⁿ-1 )**-1
		for _, q := range s.bp {
			cq := q.Coefficients()
			for j := 0; j < len(cq); j++ {
				cq[j].Mul(&cq[j], &cosetExponentiatedToNMinusOne)
			}
		}
	}

	// ——————————————————————————————————————————————————————————————————————— 启动异步“全局回滚”：把所有“按幂次相位污染”一次性撤掉
	// scale everything back
	// go func() {
	// 	s.x[id_ZS] = nil
	// 	s.x[id_Qk] = nil

	// 	var cs fr.Element
	// 	cs.Set(&shifters[0])
	// 	for i := 1; i < len(shifters); i++ {
	// 		cs.Mul(&cs, &shifters[i])
	// 	}
	// 	cs.Inverse(&cs)

	// 	batchApply(s.x, func(p *iop.Polynomial) {
	// 		if p == nil {
	// 			return
	// 		}
	// 		p.ToCanonical(s.domain0, 8).ToRegular()
	// 		scalePowers(p, cs)
	// 	})

	// 	for _, q := range s.bp {
	// 		scalePowers(q, cs)
	// 	}

	// 	close(s.chRestoreLRO)
	// }()
	// —— GPU 优化的“全局回滚”（失败会自动 CPU 回退）

	go s.scaleEverythingBackGPUorCPU(shifters, poly2idx, devX)

	// ——————————————————————————————————————————————————————————————————————— 确保所有块的 buf → cres 写入都完成；然后把 cres 封装成“大域 coset上的点值多项式（位反序布局）”返回。
	// ensure all the goroutines are done
	wgBuf.Wait()

	res := iop.NewPolynomial(&cres, iop.Form{Basis: iop.LagrangeCoset, Layout: iop.BitReverse})

	return res, nil

}

// batchInvert modifies in place vec, with vec[i]<-vec[i]^{-1}, using
// the Montgomery batch inversion trick. We don't use gnark-crypto's batchInvert
// because we want to use a buffer preallocated, to avoid wasting memory.
// /!\ it doesn't check that all vec's inputs or non zero, it is ensured by the size
// of the field /!\
func batchInvert(vec, buf []fr.Element) {
	// local function only, vec and buf are of the same size
	copy(buf, vec)
	for i := 1; i < len(vec); i++ {
		vec[i].Mul(&vec[i], &vec[i-1])
	}
	acc := vec[len(vec)-1]
	acc.Inverse(&acc)
	for i := len(vec) - 1; i > 0; i-- {
		vec[i].Mul(&acc, &vec[i-1])
		acc.Mul(&acc, &buf[i])
	}
	vec[0].Set(&acc)
}

func calculateNbTasks(n int) int {
	nbAvailableCPU := runtime.NumCPU() - n
	if nbAvailableCPU < 0 {
		nbAvailableCPU = 1
	}
	nbTasks := 1 + (nbAvailableCPU / n)
	return nbTasks
}

// batchApply executes fn on all polynomials in x except x[id_ZS] in parallel.
func batchApply(x []*iop.Polynomial, fn func(*iop.Polynomial)) {
	var wg sync.WaitGroup
	for i := 0; i < len(x); i++ {
		if i == id_ZS {
			continue
		}
		wg.Add(1)
		go func(i int) {
			fn(x[i])
			wg.Done()
		}(i)
	}
	wg.Wait()
}

// p <- <p, (1, w, .., wⁿ) >
// p is supposed to be in canonical form
func scalePowers(p *iop.Polynomial, w fr.Element) {
	var acc fr.Element
	acc.SetOne()
	cp := p.Coefficients()
	for i := 0; i < p.Size(); i++ {
		cp[i].Mul(&cp[i], &acc)
		acc.Mul(&acc, &w)
	}
}

func evaluateBlinded(p, bp *iop.Polynomial, zeta fr.Element) fr.Element {
	// Get the size of the polynomial
	n := big.NewInt(int64(p.Size()))

	var pEvaluatedAtZeta fr.Element

	// Evaluate the polynomial and blinded polynomial at zeta
	chP := make(chan struct{}, 1)
	go func() {
		pEvaluatedAtZeta = p.Evaluate(zeta)
		close(chP)
	}()

	bpEvaluatedAtZeta := bp.Evaluate(zeta)

	// Multiply the evaluated blinded polynomial by tempElement
	var t fr.Element
	one := fr.One()
	t.Exp(zeta, n).Sub(&t, &one)
	bpEvaluatedAtZeta.Mul(&bpEvaluatedAtZeta, &t)

	// Add the evaluated polynomial and the evaluated blinded polynomial
	<-chP
	pEvaluatedAtZeta.Add(&pEvaluatedAtZeta, &bpEvaluatedAtZeta)

	// Return the result
	return pEvaluatedAtZeta
}

// /!\ modifies the size
func getBlindedCoefficients(p, bp *iop.Polynomial) []fr.Element {
	cp := p.Coefficients()
	cbp := bp.Coefficients()
	cp = append(cp, cbp...)
	for i := 0; i < len(cbp); i++ {
		cp[i].Sub(&cp[i], &cbp[i])
	}
	return cp
}

// commits to a polynomial of the form b*(Xⁿ-1) where b is of small degree
func commitBlindingFactor(n int, b *iop.Polynomial, key kzg.ProvingKey) curve.G1Affine {
	cp := b.Coefficients()
	np := b.Size()

	// lo
	var tmp curve.G1Affine
	tmp.MultiExp(key.G1[:np], cp, ecc.MultiExpConfig{})

	// hi
	var res curve.G1Affine
	res.MultiExp(key.G1[n:n+np], cp, ecc.MultiExpConfig{})
	res.Sub(&res, &tmp)
	return res
}

// return a random polynomial of degree n, if n==-1 cancel the blinding
func getRandomPolynomial(n int) *iop.Polynomial {
	var a []fr.Element
	if n == -1 {
		a = make([]fr.Element, 1)
		a[0].SetZero()
	} else {
		a = make([]fr.Element, n+1)
		for i := 0; i <= n; i++ {
			a[i].SetRandom()
		}
	}
	res := iop.NewPolynomial(&a, iop.Form{
		Basis: iop.Canonical, Layout: iop.Regular})
	return res
}

func coefficients(p []*iop.Polynomial) [][]fr.Element {
	res := make([][]fr.Element, len(p))
	for i, pI := range p {
		res[i] = pI.Coefficients()
	}
	return res
}

// func commitToQuotient(h1, h2, h3 []fr.Element, proof *plonkbls12381.Proof, kzgPk kzg.ProvingKey) error {
func commitToQuotient(h1, h2, h3 []fr.Element, proof *plonkbls12381.Proof, pk *ProvingKey) error {
	// g := new(errgroup.Group)

	// g.Go(func() (err error) {
	// 	// proof.H[0], err = kzg.Commit(h1, kzgPk)
	// 	proof.H[0], err = commitOnGPUOrCPU(h1, pk, false /* monomial */)
	// 	return
	// })

	// g.Go(func() (err error) {
	// 	// proof.H[1], err = kzg.Commit(h2, kzgPk)
	// 	proof.H[1], err = commitOnGPUOrCPU(h2, pk, false /* monomial */)
	// 	return
	// })

	// g.Go(func() (err error) {
	// 	// proof.H[2], err = kzg.Commit(h3, kzgPk)
	// 	proof.H[2], err = commitOnGPUOrCPU(h3, pk, false /* monomial */)
	// 	return
	// })

	// return g.Wait()
	var err error

	start_time := time.Now()
	fmt.Printf("		commitToQuotient() || commit h1 开始\n")
	proof.H[0], err = commitOnGPUOrCPU(h1, pk, false /* monomial */)
	elapsed := time.Since(start_time)
	fmt.Printf("		commitToQuotient() || commit h1 耗时: %.6f ms\n", float64(elapsed.Nanoseconds())/1e6)
	if err != nil {
		return err
	}

	start_time = time.Now()
	proof.H[1], err = commitOnGPUOrCPU(h2, pk, false /* monomial */)
	elapsed = time.Since(start_time)
	fmt.Printf("		commitToQuotient() || commit h2 耗时: %.6f ms\n", float64(elapsed.Nanoseconds())/1e6)
	if err != nil {
		return err
	}

	start_time = time.Now()
	proof.H[2], err = commitOnGPUOrCPU(h3, pk, false /* monomial */)
	elapsed = time.Since(start_time)
	fmt.Printf("		commitToQuotient() || commit h3 耗时: %.6f ms\n", float64(elapsed.Nanoseconds())/1e6)
	if err != nil {
		return err
	}

	return nil

}

// divideByZH
// The input must be in LagrangeCoset.
// The result is in Canonical Regular. (in place using a)
func divideByZH(a *iop.Polynomial, domains [2]*fft.Domain) (*iop.Polynomial, error) {

	// check that the basis is LagrangeCoset
	if a.Basis != iop.LagrangeCoset || a.Layout != iop.BitReverse {
		return nil, errors.New("invalid form")
	}

	// prepare the evaluations of x^n-1 on the big domain's coset
	xnMinusOneInverseLagrangeCoset := evaluateXnMinusOneDomainBigCoset(domains)
	rho := int(domains[1].Cardinality / domains[0].Cardinality)

	r := a.Coefficients()
	n := uint64(len(r))
	nn := uint64(64 - bits.TrailingZeros64(n))

	start_time := time.Now()
	parallelize(len(r), func(start, end int) {
		for i := start; i < end; i++ {
			iRev := bits.Reverse64(uint64(i)) >> nn
			r[i].Mul(&r[i], &xnMinusOneInverseLagrangeCoset[int(iRev)%rho])
		}
	})
	elapsed := time.Since(start_time)
	fmt.Printf("		divideByZH() || parallelize divide 耗时: %.6f ms\n", float64(elapsed.Nanoseconds())/1e6)

	// since a is in bit reverse order, ToRegular shouldn't do anything
	// TODO: change to GPU FFT
	start_time = time.Now()
	a.ToCanonical(domains[1]).ToRegular()
	elapsed = time.Since(start_time)
	fmt.Printf("		divideByZH() || ToCanonical 耗时: %.6f ms\n", float64(elapsed.Nanoseconds())/1e6)

	return a, nil

}

// evaluateXnMinusOneDomainBigCoset evaluates Xᵐ-1 on DomainBig coset
func evaluateXnMinusOneDomainBigCoset(domains [2]*fft.Domain) []fr.Element {

	rho := domains[1].Cardinality / domains[0].Cardinality

	res := make([]fr.Element, rho)

	expo := big.NewInt(int64(domains[0].Cardinality))
	res[0].Exp(domains[1].FrMultiplicativeGen, expo)

	var t fr.Element
	t.Exp(domains[1].Generator, expo)

	one := fr.One()

	for i := 1; i < int(rho); i++ {
		res[i].Mul(&res[i-1], &t)
		res[i-1].Sub(&res[i-1], &one)
	}
	res[len(res)-1].Sub(&res[len(res)-1], &one)

	res = fr.BatchInvert(res)

	return res
}

// innerComputeLinearizedPoly computes the linearized polynomial in canonical basis.
// The purpose is to commit and open all in one ql, qr, qm, qo, qk.
// * lZeta, rZeta, oZeta are the evaluation of l, r, o at zeta
// * z is the permutation polynomial, zu is Z(μX), the shifted version of Z
// * pk is the proving key: the linearized polynomial is a linear combination of ql, qr, qm, qo, qk.
//
// The Linearized polynomial is:
//
// α²*L₁(ζ)*Z(X)
// + α*( (l(ζ)+β*s1(ζ)+γ)*(r(ζ)+β*s2(ζ)+γ)*(β*s3(X))*Z(μζ) - Z(X)*(l(ζ)+β*id1(ζ)+γ)*(r(ζ)+β*id2(ζ)+γ)*(o(ζ)+β*id3(ζ)+γ))
// + l(ζ)*Ql(X) + l(ζ)r(ζ)*Qm(X) + r(ζ)*Qr(X) + o(ζ)*Qo(X) + Qk(X) + ∑ᵢQcp_(ζ)Pi_(X)
// - Z_{H}(ζ)*((H₀(X) + ζᵐ⁺²*H₁(X) + ζ²⁽ᵐ⁺²⁾*H₂(X))
//
// /!\ blindedZCanonical is modified
func (s *instance) innerComputeLinearizedPoly(lZeta, rZeta, oZeta, alpha, beta, gamma, zeta, zu fr.Element, qcpZeta, blindedZCanonical []fr.Element, pi2Canonical [][]fr.Element, pk *ProvingKey) []fr.Element {

	// l(ζ)r(ζ)
	var rl fr.Element
	rl.Mul(&rZeta, &lZeta)

	// s1 =  α*(l(ζ)+β*s1(β)+γ)*(r(ζ)+β*s2(β)+γ)*β*Z(μζ)
	// s2 = -α*(l(ζ)+β*ζ+γ)*(r(ζ)+β*u*ζ+γ)*(o(ζ)+β*u²*ζ+γ)
	// the linearised polynomial is
	// α²*L₁(ζ)*Z(X) +
	// s1*s3(X)+s2*Z(X) + l(ζ)*Ql(X) +
	// l(ζ)r(ζ)*Qm(X) + r(ζ)*Qr(X) + o(ζ)*Qo(X) + Qk(X) + ∑ᵢQcp_(ζ)Pi_(X) -
	// Z_{H}(ζ)*((H₀(X) + ζᵐ⁺²*H₁(X) + ζ²⁽ᵐ⁺²⁾*H₂(X))
	var s1, s2 fr.Element
	chS1 := make(chan struct{}, 1)
	go func() {
		s1 = s.trace.S1.Evaluate(zeta)                       // s1(ζ)
		s1.Mul(&s1, &beta).Add(&s1, &lZeta).Add(&s1, &gamma) // (l(ζ)+β*s1(ζ)+γ)
		close(chS1)
	}()

	tmp := s.trace.S2.Evaluate(zeta)                         // s2(ζ)
	tmp.Mul(&tmp, &beta).Add(&tmp, &rZeta).Add(&tmp, &gamma) // (r(ζ)+β*s2(ζ)+γ)
	<-chS1
	s1.Mul(&s1, &tmp).Mul(&s1, &zu).Mul(&s1, &beta).Mul(&s1, &alpha) // (l(ζ)+β*s1(ζ)+γ)*(r(ζ)+β*s2(ζ)+γ)*β*Z(μζ)*α

	var uzeta, uuzeta fr.Element
	uzeta.Mul(&zeta, &pk.Vk.CosetShift)
	uuzeta.Mul(&uzeta, &pk.Vk.CosetShift)

	s2.Mul(&beta, &zeta).Add(&s2, &lZeta).Add(&s2, &gamma)      // (l(ζ)+β*ζ+γ)
	tmp.Mul(&beta, &uzeta).Add(&tmp, &rZeta).Add(&tmp, &gamma)  // (r(ζ)+β*u*ζ+γ)
	s2.Mul(&s2, &tmp)                                           // (l(ζ)+β*ζ+γ)*(r(ζ)+β*u*ζ+γ)
	tmp.Mul(&beta, &uuzeta).Add(&tmp, &oZeta).Add(&tmp, &gamma) // (o(ζ)+β*u²*ζ+γ)
	s2.Mul(&s2, &tmp)                                           // (l(ζ)+β*ζ+γ)*(r(ζ)+β*u*ζ+γ)*(o(ζ)+β*u²*ζ+γ)
	s2.Neg(&s2).Mul(&s2, &alpha)

	// Z_h(ζ), ζⁿ⁺², L₁(ζ)*α²*Z
	var zhZeta, zetaNPlusTwo, alphaSquareLagrangeZero, one, den, frNbElmt fr.Element
	one.SetOne()
	nbElmt := int64(s.domain0.Cardinality)
	alphaSquareLagrangeZero.Set(&zeta).Exp(alphaSquareLagrangeZero, big.NewInt(nbElmt)) // ζⁿ
	zetaNPlusTwo.Mul(&alphaSquareLagrangeZero, &zeta).Mul(&zetaNPlusTwo, &zeta)         // ζⁿ⁺²
	alphaSquareLagrangeZero.Sub(&alphaSquareLagrangeZero, &one)                         // ζⁿ - 1
	zhZeta.Set(&alphaSquareLagrangeZero)                                                // Z_h(ζ) = ζⁿ - 1
	frNbElmt.SetUint64(uint64(nbElmt))
	den.Sub(&zeta, &one).Inverse(&den)                           // 1/(ζ-1)
	alphaSquareLagrangeZero.Mul(&alphaSquareLagrangeZero, &den). // L₁ = (ζⁿ - 1)/(ζ-1)
									Mul(&alphaSquareLagrangeZero, &alpha).
									Mul(&alphaSquareLagrangeZero, &alpha).
									Mul(&alphaSquareLagrangeZero, &s.domain0.CardinalityInv) // α²*L₁(ζ)

	s3canonical := s.trace.S3.Coefficients()

	s.trace.Qk.ToCanonical(s.domain0).ToRegular()

	// len(h1)=len(h2)=len(blindedZCanonical)=len(h3)+1 when Statistical ZK is activated
	// len(h1)=len(h2)=len(h3)=len(blindedZCanonical)-1 when Statistical ZK is deactivated
	h1 := s.h1()
	h2 := s.h2()
	h3 := s.h3()

	// at this stage we have
	// s1 =  α*(l(ζ)+β*s1(β)+γ)*(r(ζ)+β*s2(β)+γ)*β*Z(μζ)
	// s2 = -α*(l(ζ)+β*ζ+γ)*(r(ζ)+β*u*ζ+γ)*(o(ζ)+β*u²*ζ+γ)
	parallelize(len(blindedZCanonical), func(start, end int) {

		cql := s.trace.Ql.Coefficients()
		cqr := s.trace.Qr.Coefficients()
		cqm := s.trace.Qm.Coefficients()
		cqo := s.trace.Qo.Coefficients()
		cqk := s.trace.Qk.Coefficients()

		var t, t0, t1 fr.Element

		for i := start; i < end; i++ {
			t.Mul(&blindedZCanonical[i], &s2) // -Z(X)*α*(l(ζ)+β*ζ+γ)*(r(ζ)+β*u*ζ+γ)*(o(ζ)+β*u²*ζ+γ)
			if i < len(s3canonical) {
				t0.Mul(&s3canonical[i], &s1) // α*(l(ζ)+β*s1(β)+γ)*(r(ζ)+β*s2(β)+γ)*β*Z(μζ)*β*s3(X)
				t.Add(&t, &t0)
			}
			if i < len(cqm) {
				t1.Mul(&cqm[i], &rl)     // l(ζ)r(ζ)*Qm(X)
				t.Add(&t, &t1)           // linPol += l(ζ)r(ζ)*Qm(X)
				t0.Mul(&cql[i], &lZeta)  // l(ζ)Q_l(X)
				t.Add(&t, &t0)           // linPol += l(ζ)*Ql(X)
				t0.Mul(&cqr[i], &rZeta)  //r(ζ)*Qr(X)
				t.Add(&t, &t0)           // linPol += r(ζ)*Qr(X)
				t0.Mul(&cqo[i], &oZeta)  // o(ζ)*Qo(X)
				t.Add(&t, &t0)           // linPol += o(ζ)*Qo(X)
				t.Add(&t, &cqk[i])       // linPol += Qk(X)
				for j := range qcpZeta { // linPol += ∑ᵢQcp_(ζ)Pi_(X)
					t0.Mul(&pi2Canonical[j][i], &qcpZeta[j])
					t.Add(&t, &t0)
				}
			}

			t0.Mul(&blindedZCanonical[i], &alphaSquareLagrangeZero) // α²L₁(ζ)Z(X)
			blindedZCanonical[i].Add(&t, &t0)                       // linPol += α²L₁(ζ)Z(X)

			// if statistical zeroknowledge is deactivated, len(h1)=len(h2)=len(h3)=len(blindedZ)-1.
			// Else len(h1)=len(h2)=len(blindedZCanonical)=len(h3)+1
			if i < len(h3) {
				t.Mul(&h3[i], &zetaNPlusTwo).
					Add(&t, &h2[i]).
					Mul(&t, &zetaNPlusTwo).
					Add(&t, &h1[i]).
					Mul(&t, &zhZeta)
				blindedZCanonical[i].Sub(&blindedZCanonical[i], &t) // linPol -= Z_h(ζ)*(H₀(X) + ζᵐ⁺²*H₁(X) + ζ²⁽ᵐ⁺²⁾*H₂(X))
			} else {
				if s.opt.StatisticalZK {
					t.Mul(&h2[i], &zetaNPlusTwo).
						Add(&t, &h1[i]).
						Mul(&t, &zhZeta)
					blindedZCanonical[i].Sub(&blindedZCanonical[i], &t) // linPol -= Z_h(ζ)*(H₀(X) + ζᵐ⁺²*H₁(X) + ζ²⁽ᵐ⁺²⁾*H₂(X))
				}
			}
		}
	})

	return blindedZCanonical
}

var errContextDone = errors.New("context done")

// func BatchOpenSinglePoint(polynomials [][]fr.Element, digests []kzg.Digest, point fr.Element, pk kzg.ProvingKey, dataTranscript fr.Element) (kzg.BatchOpeningProof, error) {
func BatchOpenSinglePoint(polynomials [][]fr.Element, digests []kzg.Digest, point fr.Element, pk *ProvingKey, dataTranscript fr.Element) (kzg.BatchOpeningProof, error) {

	// check for invalid sizes
	nbDigests := len(digests)
	if nbDigests != len(polynomials) {
		return kzg.BatchOpeningProof{}, kzg.ErrInvalidNbDigests
	}

	// TODO ensure the polynomials are of the same size
	largestPoly := -1
	for _, p := range polynomials {
		// if len(p) == 0 || len(p) > len(pk.G1) {
		if len(p) == 0 || len(p) > len(pk.Kzg.G1) {
			return kzg.BatchOpeningProof{}, kzg.ErrInvalidPolynomialSize
		}
		if len(p) > largestPoly {
			largestPoly = len(p)
		}
	}

	var res kzg.BatchOpeningProof

	// compute the purported values
	res.ClaimedValues = make([]fr.Element, len(polynomials))
	var wg sync.WaitGroup
	wg.Add(len(polynomials))
	for i := 0; i < len(polynomials); i++ {
		go func(_i int) {
			res.ClaimedValues[_i] = eval(polynomials[_i], point)
			wg.Done()
		}(i)
	}

	// wait for polynomial evaluations to be completed (res.ClaimedValues)
	wg.Wait()

	// derive the challenge γ, binded to the point and the commitments
	gamma, err := deriveGamma(point, digests, res.ClaimedValues, dataTranscript)
	if err != nil {
		return kzg.BatchOpeningProof{}, err
	}

	// ∑ᵢγⁱf(a)
	var foldedEvaluations fr.Element
	chSumGammai := make(chan struct{}, 1)
	go func() {
		foldedEvaluations = res.ClaimedValues[nbDigests-1]
		for i := nbDigests - 2; i >= 0; i-- {
			foldedEvaluations.Mul(&foldedEvaluations, &gamma).
				Add(&foldedEvaluations, &res.ClaimedValues[i])
		}
		close(chSumGammai)
	}()

	// compute ∑ᵢγⁱfᵢ
	// note: if we are willing to parallelize that, we could clone the poly and scale them by
	// gamma n in parallel, before reducing into foldedPolynomials
	foldedPolynomials := make([]fr.Element, largestPoly)
	copy(foldedPolynomials, polynomials[0])
	gammas := make([]fr.Element, len(polynomials))
	gammas[0] = gamma
	for i := 1; i < len(polynomials); i++ {
		gammas[i].Mul(&gammas[i-1], &gamma)
	}

	for i := 1; i < len(polynomials); i++ {
		i := i
		parallelize(len(polynomials[i]), func(start, end int) {
			var pj fr.Element
			for j := start; j < end; j++ {
				pj.Mul(&polynomials[i][j], &gammas[i-1])
				foldedPolynomials[j].Add(&foldedPolynomials[j], &pj)
			}
		})
	}

	// compute H
	<-chSumGammai
	h := dividePolyByXminusA(foldedPolynomials, foldedEvaluations, point)
	foldedPolynomials = nil // same memory as h

	// res.H, err = kzg.Commit(h, pk)
	res.H, err = commitOnGPUOrCPU(h, pk, false /* monomial */)
	if err != nil {
		return kzg.BatchOpeningProof{}, err
	}

	return res, nil
}

func FoldProof(digests []kzg.Digest, batchOpeningProof *kzg.BatchOpeningProof, point fr.Element, dataTranscript fr.Element) (kzg.OpeningProof, kzg.Digest, error) {

	nbDigests := len(digests)

	// check consistency between numbers of claims vs number of digests
	if nbDigests != len(batchOpeningProof.ClaimedValues) {
		return kzg.OpeningProof{}, kzg.Digest{}, kzg.ErrInvalidNbDigests
	}

	// derive the challenge γ, binded to the point and the commitments
	gamma, err := deriveGamma(point, digests, batchOpeningProof.ClaimedValues, dataTranscript)
	if err != nil {
		return kzg.OpeningProof{}, kzg.Digest{}, kzg.ErrInvalidNbDigests
	}

	// fold the claimed values and digests
	// gammai = [1,γ,γ²,..,γⁿ⁻¹]
	gammai := make([]fr.Element, nbDigests)
	gammai[0].SetOne()
	if nbDigests > 1 {
		gammai[1] = gamma
	}
	for i := 2; i < nbDigests; i++ {
		gammai[i].Mul(&gammai[i-1], &gamma)
	}

	foldedDigests, foldedEvaluations, err := fold(digests, batchOpeningProof.ClaimedValues, gammai)
	if err != nil {
		return kzg.OpeningProof{}, kzg.Digest{}, err
	}

	// create the folded opening proof
	var res kzg.OpeningProof
	res.ClaimedValue.Set(&foldedEvaluations)
	res.H.Set(&batchOpeningProof.H)

	return res, foldedDigests, nil
}

func eval(p []fr.Element, point fr.Element) fr.Element {
	var res fr.Element
	n := len(p)
	res.Set(&p[n-1])
	for i := n - 2; i >= 0; i-- {
		res.Mul(&res, &point).Add(&res, &p[i])
	}
	return res
}

func dividePolyByXminusA(f []fr.Element, fa, a fr.Element) []fr.Element {

	// first we compute f-f(a)
	f[0].Sub(&f[0], &fa)

	// now we use synthetic division to divide by x-a
	var t fr.Element
	for i := len(f) - 2; i >= 0; i-- {
		t.Mul(&f[i+1], &a)

		f[i].Add(&f[i], &t)
	}

	// the result is of degree deg(f)-1
	return f[1:]
}

func deriveGamma(point fr.Element, digests []kzg.Digest, claimedValues []fr.Element, dataTranscript ...fr.Element) (fr.Element, error) {

	// derive the challenge gamma, binded to the point and the commitments
	fs := NewTranscript(eon.CID_GAMMA)
	if err := fs.Bind(eon.CID_GAMMA, point); err != nil {
		return fr.Element{}, err
	}
	for i := range digests {
		if err := fs.Bind(eon.CID_GAMMA, eon.HashG1(digests[i])); err != nil {
			return fr.Element{}, err
		}
	}
	for i := range claimedValues {
		if err := fs.Bind(eon.CID_GAMMA, claimedValues[i]); err != nil {
			return fr.Element{}, err
		}
	}

	for i := 0; i < len(dataTranscript); i++ {
		if err := fs.Bind(eon.CID_GAMMA, dataTranscript[i]); err != nil {
			return fr.Element{}, err
		}
	}

	gamma, err := fs.ComputeChallenge(eon.CID_GAMMA)
	if err != nil {
		return fr.Element{}, err
	}

	return gamma, nil
}

func fold(di []kzg.Digest, fai []fr.Element, ci []fr.Element) (kzg.Digest, fr.Element, error) {

	// length inconsistency between digests and evaluations should have been done before calling this function
	nbDigests := len(di)

	// fold the claimed values ∑ᵢcᵢf(aᵢ)
	var foldedEvaluations, tmp fr.Element
	for i := 0; i < nbDigests; i++ {
		tmp.Mul(&fai[i], &ci[i])
		foldedEvaluations.Add(&foldedEvaluations, &tmp)
	}

	// fold the digests ∑ᵢ[cᵢ]([fᵢ(α)]G₁)
	var foldedDigests kzg.Digest
	_, err := foldedDigests.MultiExp(di, ci, ecc.MultiExpConfig{})
	if err != nil {
		return foldedDigests, foldedEvaluations, err
	}

	// folding done
	return foldedDigests, foldedEvaluations, nil

}

var (
	errChallengeNotFound            = errors.New("challenge not recorded in the transcript")
	errChallengeAlreadyComputed     = errors.New("challenge already computed, cannot be binded to other values")
	errPreviousChallengeNotComputed = errors.New("the previous challenge is needed and has not been computed")
)

// Transcript handles the creation of challenges for Fiat Shamir.
type Transcript struct {
	challenges map[fr.Element]challenge
	previous   *challenge
}

type challenge struct {
	position   int            // position of the challenge in the Transcript. order matters.
	bindings   [][]fr.Element // bindings stores the variables a challenge is binded to.
	value      fr.Element     // value stores the computed challenge
	isComputed bool
}

// NewTranscript returns a new transcript.
// h is the hash function that is used to compute the challenges.
// challenges are the name of the challenges. The order of the challenges IDs matters.
func NewTranscript(challengesID ...fr.Element) *Transcript {
	challenges := make(map[fr.Element]challenge)
	for i := range challengesID {
		challenges[challengesID[i]] = challenge{position: i}
	}
	t := &Transcript{
		challenges: challenges,
	}
	return t
}

// Bind binds the challenge to value. A challenge can be binded to an
// arbitrary number of values, but the order in which the binded values
// are added is important. Once a challenge is computed, it cannot be
// binded to other values.
func (t *Transcript) Bind(challengeID fr.Element, bValue ...fr.Element) error {

	currentChallenge, ok := t.challenges[challengeID]
	if !ok {
		return errChallengeNotFound
	}

	if currentChallenge.isComputed {
		return errChallengeAlreadyComputed
	}

	bCopy := make([]fr.Element, len(bValue))
	copy(bCopy, bValue)
	currentChallenge.bindings = append(currentChallenge.bindings, bCopy)
	t.challenges[challengeID] = currentChallenge

	return nil

}

// ComputeChallenge computes the challenge corresponding to the given name.
// The challenge is:
// * H(name || previous_challenge || binded_values...) if the challenge is not the first one
// * H(name || binded_values... ) if it is the first challenge
func (t *Transcript) ComputeChallenge(challengeID fr.Element) (fr.Element, error) {
	challenge, ok := t.challenges[challengeID]
	if !ok {
		return fr.Element{}, errChallengeNotFound
	}

	// if the challenge was already computed we return it
	if challenge.isComputed {
		return challenge.value, nil
	}

	// reset before populating the internal state
	resfrom := []fr.Element{}

	resfrom = append(resfrom, challengeID)

	// write the previous challenge if it's not the first challenge
	if challenge.position != 0 {
		if t.previous == nil || (t.previous.position != challenge.position-1) {
			return fr.Element{}, errPreviousChallengeNotComputed
		}
		resfrom = append(resfrom, t.previous.value)
	}

	// write the binded values in the order they were added
	for _, b := range challenge.bindings {
		resfrom = append(resfrom, b...)
	}

	// compute the hash of the accumulated values
	res := hashsum(resfrom...)

	challenge.value = res
	challenge.isComputed = true

	t.challenges[challengeID] = challenge
	t.previous = &challenge

	return res, nil

}

const WIDTH = 2
const ROuND_FULL = 8
const ROUND_PARTIAL = 56

var GetPermutation = sync.OnceValue(func() *poseidon2.Permutation {
	return poseidon2.NewPermutationWithSeed(WIDTH, ROuND_FULL, ROUND_PARTIAL, "EON_POSEIDON2_HASH_SEED")
})

func compress(x, y fr.Element) fr.Element {
	vars := [2]fr.Element{x, y}
	if err := GetPermutation().Permutation(vars[:]); err != nil {
		log.Fatalln(err)
	}
	var ret fr.Element
	ret.Add(&vars[1], &y)
	return ret
}

func hashsum(val ...fr.Element) fr.Element {
	var ret fr.Element
	for _, v := range val {
		ret = compress(ret, v)
	}
	return ret
}

func parallelize(nbIterations int, work func(int, int), maxCpus ...int) {

	nbTasks := runtime.NumCPU()
	if len(maxCpus) == 1 {
		nbTasks = maxCpus[0]
	}
	nbIterationsPerCpus := nbIterations / nbTasks

	// more CPUs than tasks: a CPU will work on exactly one iteration
	if nbIterationsPerCpus < 1 {
		nbIterationsPerCpus = 1
		nbTasks = nbIterations
	}

	var wg sync.WaitGroup

	extraTasks := nbIterations - (nbTasks * nbIterationsPerCpus)
	extraTasksOffset := 0

	for i := 0; i < nbTasks; i++ {
		wg.Add(1)
		_start := i*nbIterationsPerCpus + extraTasksOffset
		_end := _start + nbIterationsPerCpus
		if extraTasks > 0 {
			_end++
			extraTasks--
			extraTasksOffset++
		}
		go func() {
			work(_start, _end)
			wg.Done()
		}()
	}

	wg.Wait()
}
func Dev_bindPublicData(fs *Transcript, challenge fr.Element, vk *plonkbls12381.VerifyingKey, publicInputs []fr.Element) error {

	// permutation
	if err := fs.Bind(challenge, eon.HashG1(vk.S[0])); err != nil {
		return err
	}
	if err := fs.Bind(challenge, eon.HashG1(vk.S[1])); err != nil {
		return err
	}
	if err := fs.Bind(challenge, eon.HashG1(vk.S[2])); err != nil {
		return err
	}

	// coefficients
	if err := fs.Bind(challenge, eon.HashG1(vk.Ql)); err != nil {
		return err
	}
	if err := fs.Bind(challenge, eon.HashG1(vk.Qr)); err != nil {
		return err
	}
	if err := fs.Bind(challenge, eon.HashG1(vk.Qm)); err != nil {
		return err
	}
	if err := fs.Bind(challenge, eon.HashG1(vk.Qo)); err != nil {
		return err
	}
	if err := fs.Bind(challenge, eon.HashG1(vk.Qk)); err != nil {
		return err
	}
	for i := range vk.Qcp {
		if err := fs.Bind(challenge, eon.HashG1(vk.Qcp[i])); err != nil {
			return err
		}
	}

	return nil

}

func Dev_deriveRandomness(fs *Transcript, challenge fr.Element, points ...*curve.G1Affine) (fr.Element, error) {
	for _, p := range points {
		if err := fs.Bind(challenge, eon.HashG1(*p)); err != nil {
			return fr.Element{}, err
		}
	}
	b, err := fs.ComputeChallenge(challenge)
	if err != nil {
		return fr.Element{}, err
	}
	return b, nil
}

func commitOnGPUOrCPU(coeffs []fr.Element, pk *ProvingKey, useLagrange bool) (curve.G1Affine, error) {
	// GPU
	if HasIcicle && pk != nil && pk.deviceInfo != nil {
		var dig kzg.Digest
		var st icicle_runtime.EIcicleError

		done := make(chan struct{})
		icicle_runtime.RunOnDevice(&pk.deviceInfo.Device, func(args ...any) {
			defer close(done)
			if useLagrange {
				// dig, st = kzg_bls12_381.OnDeviceCommit(coeffs, pk.deviceInfo.G1Device.G1Lagrange)
				base := pk.deviceInfo.G1Device.G1Lagrange.RangeTo(len(coeffs), false)
				dig, st = kzg_bls12_381.OnDeviceCommit(coeffs, base)
			} else {
				// dig, st = kzg_bls12_381.OnDeviceCommit(coeffs, pk.deviceInfo.G1Device.G1)
				base := pk.deviceInfo.G1Device.G1.RangeTo(len(coeffs), false)
				dig, st = kzg_bls12_381.OnDeviceCommit(coeffs, base)
			}
		})
		<-done

		if st == icicle_runtime.Success {
			return curve.G1Affine(dig), nil
		}
		log.Printf("[GPU failed -> CPU] kzg.Commit")
	}

	// CPU
	if useLagrange {
		return kzg.Commit(coeffs, pk.KzgLagrange)
	}
	return kzg.Commit(coeffs, pk.Kzg)
}

// commits to a polynomial of the form b*(Xⁿ-1) where b is of small degree
// Prefer GPU (icicle v3); fallback to CPU if GPU unavailable or returns error.
func commitBlindingFactorGPUOrCPU(n int, b *iop.Polynomial, pk *ProvingKey) (curve.G1Affine, error) {
	cp := b.Coefficients()
	np := b.Size()

	// --- GPU path ---
	if HasIcicle && pk != nil && pk.deviceInfo != nil {
		var (
			lo, hi     kzg.Digest
			stLo, stHi icicle_runtime.EIcicleError
		)

		done := make(chan struct{})
		icicle_runtime.RunOnDevice(&pk.deviceInfo.Device, func(args ...any) {
			defer close(done)

			// bases for lo: G1[0:np]
			baseLo := pk.deviceInfo.G1Device.G1.RangeTo(np, false)

			// bases for hi: G1[n:n+np]
			baseHi := pk.deviceInfo.G1Device.G1.Range(n, n+np, false)

			lo, stLo = kzg_bls12_381.OnDeviceCommit(cp, baseLo)
			if stLo == icicle_runtime.Success {
				hi, stHi = kzg_bls12_381.OnDeviceCommit(cp, baseHi)
			}
		})
		<-done

		if stLo == icicle_runtime.Success && stHi == icicle_runtime.Success {
			res := curve.G1Affine(hi)
			tmp := curve.G1Affine(lo)
			res.Sub(&res, &tmp)
			return res, nil
		}

		log.Printf("[GPU failed -> CPU] commit blinding factor (lo=%s hi=%s)", stLo.AsString(), stHi.AsString())
	}

	// --- CPU fallback ---
	return commitBlindingFactor(n, b, pk.Kzg), nil
}

func OpenOnGPUOrCPU(p []fr.Element, point fr.Element, pk *ProvingKey) (kzg.OpeningProof, error) {
	// 尝试 GPU
	if HasIcicle && pk != nil && pk.deviceInfo != nil {
		var pr kzg.OpeningProof
		var st icicle_runtime.EIcicleError

		done := make(chan struct{})
		icicle_runtime.RunOnDevice(&pk.deviceInfo.Device, func(args ...any) {
			defer close(done)
			// 传入 monomial SRS（和 Commit 一致）
			pr, st = kzg_bls12_381.OnDeviceOpen(p, point, pk.deviceInfo.G1Device.G1)
		})
		<-done

		if st == icicle_runtime.Success {
			return pr, nil
		}
		log.Printf("[GPU failed -> CPU] kzg.Open: %s", st.AsString())
	}

	// CPU 回退
	return kzg.Open(p, point, pk.Kzg)
}

// 将多项式 p 变换到“当前 coset 上的拉格朗日点值（Regular 布局）”
// 版本：GPU 端直接使用“已在显存中的缩放向量 wDev”；失败则回退到 CPU。
func (s *instance) toCosetLagrangeOnGPUorCPU_DEV(
	p *iop.Polynomial,
	wDevReg, wDevRev icicle_core.DeviceSlice,
	sk scalingKind,
	xdev *icicle_core.DeviceSlice,
) error {
	if p == nil {
		return nil
	}

	selW := wDevReg
	if p.Layout == iop.BitReverse {
		selW = wDevRev
	}

	// ---------- GPU 路径 ----------
	if HasIcicle && s.pk != nil && s.pk.deviceInfo != nil && xdev != nil {
		coeffs := p.Coefficients()

		var st icicle_runtime.EIcicleError
		var gpuErr error

		s.pk.deviceInfo.mu.Lock() // 串行化设备操作，避免跨 device 的 slice 冲突
		defer s.pk.deviceInfo.mu.Unlock()

		done := make(chan struct{})
		// startTotal := time.Now()
		icicle_runtime.RunOnDevice(&s.pk.deviceInfo.Device, func(args ...any) {
			defer close(done)
			dev := *xdev

			// 约定：每次调用结束前把 dev 恢复为 Canonical（见尾部 INTT），
			// 因此这里 dev 一定是 Canonical。

			if st = kzg_bls12_381.MontConvOnDevice(dev, false); st != icicle_runtime.Success {
				gpuErr = fmt.Errorf("MontConv(dev->nonMont) failed: %s", st.AsString())
				return
			}
			if st = kzg_bls12_381.VecMulOnDevice(dev, selW); st != icicle_runtime.Success {
				gpuErr = fmt.Errorf("VecMulOnDevice failed: %s", st.AsString())
				return
			}
			if st = kzg_bls12_381.MontConvOnDevice(dev, true); st != icicle_runtime.Success {
				gpuErr = fmt.Errorf("MontConv(dev->Mont) failed: %s", st.AsString())
				return
			}

			// 正变换 NTT：Canonical -> Lagrange(小域)
			if st = kzg_bls12_381.NttOnDevice(dev); st != icicle_runtime.Success {
				gpuErr = fmt.Errorf("NttOnDevice failed: %s", st.AsString())
				return
			}

			// 4) 回拷 + 释放
			host := icicle_core.HostSliceFromElements(coeffs)
			host.CopyFromDevice(&dev)

			// 4) 立刻把 dev 恢复为 Canonical，方便下一个 coset 继续复用
			if st = kzg_bls12_381.INttOnDevice(dev); st != icicle_runtime.Success {
				gpuErr = fmt.Errorf("INttOnDevice (restore canonical) failed: %s", st.AsString())
				return
			}
		})
		<-done
		// totalElapsed := time.Since(startTotal)
		// fmt.Printf("		[Timing] toCosetLagrange 总耗时: %.6f ms (%.2f s)\n",
		// 	float64(totalElapsed.Nanoseconds())/1e6, float64(totalElapsed.Nanoseconds())/1e9)

		if gpuErr == nil {
			// 和原 CPU 逻辑保持一致：本轮后 p 处于 Lagrange Regular
			*p = *iop.NewPolynomial(&coeffs, iop.Form{Basis: iop.Lagrange, Layout: iop.Regular})
			return nil
		}
		log.Printf("[GPU failed -> CPU] %v", gpuErr)
	}
	// ---------------- CPU 回退（保持原有逻辑） ----------------
	nbTasks := calculateNbTasks(len(s.x)-1) * 2
	p.ToCanonical(s.domain0, nbTasks)

	// CPU 路径需要 host 侧的缩放表
	var w []fr.Element
	switch sk {
	case scaleCoset:
		reg, rev := s.pk.deviceInfo.ensureHostCosetTables(s.domain0)
		if p.Layout == iop.Regular {
			w = reg
		} else {
			w = rev
		}
	case scaleBig:
		reg, rev := s.pk.deviceInfo.ensureHostBigTables(s.domain0.Cardinality)
		if p.Layout == iop.Regular {
			w = reg
		} else {
			w = rev
		}
	default:
		return fmt.Errorf("unknown scaling kind")
	}

	cp := p.Coefficients()
	parallelize(len(cp), func(start, end int) {
		for j := start; j < end; j++ {
			cp[j].Mul(&cp[j], &w[j])
		}
	}, nbTasks)

	p.ToLagrange(s.domain0, nbTasks).ToRegular()
	return nil
}

type scalingKind int

const (
	scaleCoset scalingKind = iota // 使用 coset 表（首块）
	scaleBig                      // 使用大域 w^j 表（其余块）
)

// GPU/CPU 二选一的“全局回滚”实现：把所有多项式的系数乘以 cs^j，并统一回到 Canonical+Regular。
//
// 注意：
//   - 必须跳过 ZS（id_ZS）以及已经置 nil 的 Qk（id_Qk）
//   - 尽可能使用 GPU；失败则回退 CPU
//   - 语义与参考实现一致：在 CPU fallback 中仍然是
//     p.ToCanonical(s.domain0, 8).ToRegular() 再做 cs^j 缩放
//     在 GPU 路径中：用 INTT 把 Lagrange 评估值变回 Canonical 系数，再做 cs^j 缩放。
func (s *instance) scaleEverythingBackGPUorCPU(
	shifters []fr.Element,
	_ map[*iop.Polynomial]int, // 现在不再依赖 poly2idx/devX，这两个参数可以忽略
	_ []icicle_core.DeviceSlice,
) {
	// 1. 和原逻辑保持一致：先把 ZS / Qk 清掉
	s.x[id_ZS] = nil
	s.x[id_Qk] = nil

	// 2. 计算 cs = (∏ shifters)^(-1)
	var cs fr.Element
	cs.Set(&shifters[0])
	for i := 1; i < len(shifters); i++ {
		cs.Mul(&cs, &shifters[i])
	}
	cs.Inverse(&cs)

	// 3. 预计算 cs^j：accList[j] = cs^j，长度 = |domain0|
	n := int(s.domain0.Cardinality)
	accList := make([]fr.Element, n)
	{
		var acc fr.Element
		acc.SetOne()
		for j := 0; j < n; j++ {
			accList[j].Set(&acc)
			acc.Mul(&acc, &cs)
		}
	}

	useGPU := HasIcicle && s.pk != nil && s.pk.deviceInfo != nil

	if useGPU {
		var gpuErr error
		done := make(chan struct{})

		s.pk.deviceInfo.mu.Lock()
		icicle_runtime.RunOnDevice(&s.pk.deviceInfo.Device, func(args ...any) {
			defer s.pk.deviceInfo.mu.Unlock()
			defer close(done)

			// 3.1 把 accList 上传到 GPU，并转为非 Montgomery，供 VecMul 使用
			hostW := icicle_core.HostSliceFromElements(accList)
			var wDevFull icicle_core.DeviceSlice
			hostW.CopyToDevice(&wDevFull, true)
			defer wDevFull.Free()

			if st := kzg_bls12_381.MontConvOnDevice(wDevFull, false /* FromMontgomery */); st != icicle_runtime.Success {
				gpuErr = fmt.Errorf("FromMontgomery(accList) failed: %s", st.AsString())
				return
			}

			// 3.2 遍历所有多项式：先用 GPU INTT 把 Lagrange 评估值变成 Canonical 系数，再乘 cs^j
			for idx := 0; idx < len(s.x); idx++ {
				if idx == id_ZS {
					continue
				}
				p := s.x[idx]
				if p == nil {
					continue
				}

				coeffs := p.Coefficients()
				deg := len(coeffs)
				if deg == 0 {
					continue
				}

				// 只用 cs^0..cs^{deg-1}
				wDev := wDevFull.RangeTo(deg, false)

				// 把当前 poly 的“系数/评估值”上传到设备
				hostP := icicle_core.HostSliceFromElements(coeffs)
				var dev icicle_core.DeviceSlice
				hostP.CopyToDevice(&dev, true)

				// (1) 用 GPU INTT 把 Lagrange 评估值 -> Canonical 系数
				if st := kzg_bls12_381.INttOnDevice(dev); st != icicle_runtime.Success {
					gpuErr = fmt.Errorf("poly[%d] INTT failed: %s", idx, st.AsString())
					dev.Free()
					return
				}
				p.Form.Basis = iop.Canonical
				p.Form.Layout = iop.Regular

				// (2) 在 Canonical 系数上做 cs^j 缩放（VecOps 期望非 Montgomery）
				if st := kzg_bls12_381.MontConvOnDevice(dev, false /* FromMontgomery */); st != icicle_runtime.Success {
					gpuErr = fmt.Errorf("poly[%d] FromMontgomery: %s", idx, st.AsString())
					dev.Free()
					return
				}

				if st := kzg_bls12_381.VecMulOnDevice(dev, wDev); st != icicle_runtime.Success {
					gpuErr = fmt.Errorf("poly[%d] VecMul: %s", idx, st.AsString())
					dev.Free()
					return
				}
				if st := kzg_bls12_381.MontConvOnDevice(dev, true /* ToMontgomery */); st != icicle_runtime.Success {
					gpuErr = fmt.Errorf("poly[%d] ToMontgomery: %s", idx, st.AsString())
					dev.Free()
					return
				}

				// (3) 回拷到 host：覆盖原来的数据
				hostP.CopyFromDevice(&dev)
				dev.Free()

				// 这里不再调用 p.ToCanonical/ToRegular：
				//   - 值语义上，已经是 canonical + regular 对应的系数；
				//   - Form 元数据如果后面没有再做 Basis 变换，是安全的。
			}
		})
		<-done

		if gpuErr != nil {
			log.Printf("[GPU failed -> CPU] scaleEverythingBack: %v", gpuErr)
			useGPU = false
		}
	}

	// 4. CPU fallback：严格按照参考实现语义：
	//    对每个 poly 先 ToCanonical(s.domain0).ToRegular()，再做 cs^j 缩放。
	if !useGPU {
		batchApply(s.x, func(p *iop.Polynomial) {
			if p == nil {
				return
			}
			p.ToCanonical(s.domain0, 8).ToRegular()
			scalePowers(p, cs)
		})
	} else {
		// GPU 路径中，coeff 已经在 canonical 系数上乘过 cs^j，这里不再做任何额外缩放。
	}

	// 5. blinding 多项式也要乘 cs^j（参考实现是用 CPU 做的）
	for _, q := range s.bp {
		if q == nil {
			continue
		}
		scalePowers(q, cs)
	}

	close(s.chRestoreLRO)
}
