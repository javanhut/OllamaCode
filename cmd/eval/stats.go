package main

import (
	"math"
	"math/rand/v2"
	"slices"
)

const z95 = 1.959963984540054

// interval is a point estimate with a 95% confidence interval. Valid is false
// when the method is undefined for the sample (e.g. a clustered SE needs at
// least two tasks); Lower/Upper are then zero and must not be used.
type interval struct {
	Method string  `json:"method"`
	Point  float64 `json:"point"`
	Lower  float64 `json:"lower"`
	Upper  float64 `json:"upper"`
	SE     float64 `json:"se,omitempty"`
	N      int     `json:"n"`
	Valid  bool    `json:"valid"`
}

// wilsonInterval is the Wilson score interval for c successes in n Bernoulli
// trials. It treats every trial as independent, which trials of the same task
// are not, so it is typically too narrow here; it is reported as a reference.
func wilsonInterval(c, n int) interval {
	iv := interval{Method: "wilson_trials", N: n}
	if n <= 0 {
		return iv
	}
	p := float64(c) / float64(n)
	nf := float64(n)
	z2 := z95 * z95
	denom := 1 + z2/nf
	center := (p + z2/(2*nf)) / denom
	half := z95 * math.Sqrt(p*(1-p)/nf+z2/(4*nf*nf)) / denom
	iv.Point, iv.Valid = p, true
	iv.Lower, iv.Upper = math.Max(0, center-half), math.Min(1, center+half)
	return iv
}

// clusteredInterval is the task-clustered normal interval: per-task pass rates
// are the unit of analysis, the point estimate is their mean and the standard
// error is sd(p_i)/sqrt(T). Trials of one task are correlated, so tasks (not
// trials) are the independent draws.
func clusteredInterval(taskRates []float64) interval {
	t := len(taskRates)
	iv := interval{Method: "task_clustered", N: t}
	if t == 0 {
		return iv
	}
	mean := meanOf(taskRates)
	iv.Point = mean
	if t < 2 {
		return iv
	}
	var ss float64
	for _, p := range taskRates {
		ss += (p - mean) * (p - mean)
	}
	iv.SE = math.Sqrt(ss/float64(t-1)) / math.Sqrt(float64(t))
	iv.Lower, iv.Upper = math.Max(0, mean-z95*iv.SE), math.Min(1, mean+z95*iv.SE)
	iv.Valid = true
	return iv
}

func meanOf(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var s float64
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

// passHatK is the unbiased estimate of P(all k trials succeed) from c
// successes in n trials: C(c,k)/C(n,k) (tau-bench's pass^k). Requires k <= n.
func passHatK(c, n, k int) float64 {
	if k < 0 || k > n || c < k {
		return 0
	}
	r := 1.0
	for i := range k {
		r *= float64(c-i) / float64(n-i)
	}
	return r
}

// passAtK is the unbiased estimate of P(at least one of k trials succeeds):
// 1 - C(n-c,k)/C(n,k). Requires k <= n.
func passAtK(c, n, k int) float64 {
	if k < 0 || k > n {
		return 0
	}
	if n-c < k {
		return 1
	}
	r := 1.0
	for i := range k {
		r *= float64(n-c-i) / float64(n-i)
	}
	return 1 - r
}

type kPoint struct {
	K       int     `json:"k"`
	PassAtK float64 `json:"pass_at_k"`
	PassHat float64 `json:"pass_hat_k"`
}

// evalStats is the uncertainty block stored in every report. All figures are
// outcome-based (behavior check), independent of the tool-contract axis.
type evalStats struct {
	Basis     string     `json:"basis"`
	Tasks     int        `json:"tasks"`
	Trials    int        `json:"trials"`
	K         int        `json:"k"`
	Successes int        `json:"successes"`
	Pass1     float64    `json:"pass_1"`
	PassAtK   float64    `json:"pass_at_k"`
	PassHatK  float64    `json:"pass_hat_k"`
	Clustered interval   `json:"ci_task_clustered"`
	Wilson    interval   `json:"ci_wilson_trials"`
	Curve     []kPoint   `json:"pass_k_curve,omitempty"`
	TaskRates []taskRate `json:"-"`
}

type taskRate struct {
	Task string
	C, N int
}

func (t taskRate) rate() float64 { return float64(t.C) / float64(t.N) }

// outcomeOf is the headline grading axis: did the workspace/output check pass.
// Tool-contract results are reported separately.
func outcomeOf(r runResult) bool { return r.BehaviorPassed }

// taskRates groups results by task (first-seen order) using the outcome axis.
func taskRates(results []runResult) []taskRate {
	idx := map[string]int{}
	var out []taskRate
	for _, r := range results {
		i, ok := idx[r.Task]
		if !ok {
			i = len(out)
			idx[r.Task] = i
			out = append(out, taskRate{Task: r.Task})
		}
		out[i].N++
		if outcomeOf(r) {
			out[i].C++
		}
	}
	return out
}

// computeStats derives intervals and pass@k / pass^k. k is the minimum trial
// count over tasks (the trials-per-task of a uniform run); pass@k/pass^k are
// averaged over tasks.
func computeStats(results []runResult) evalStats {
	rates := taskRates(results)
	st := evalStats{Basis: "outcome (behavior check)", Tasks: len(rates), Trials: len(results), TaskRates: rates}
	if len(rates) == 0 {
		return st
	}
	st.K = rates[0].N
	var pr []float64
	for _, t := range rates {
		st.K = min(st.K, t.N)
		st.Successes += t.C
		pr = append(pr, t.rate())
	}
	st.Pass1 = meanOf(pr)
	st.Clustered = clusteredInterval(pr)
	st.Wilson = wilsonInterval(st.Successes, st.Trials)
	for k := 1; k <= st.K; k++ {
		var at, hat []float64
		for _, t := range rates {
			at = append(at, passAtK(t.C, t.N, k))
			hat = append(hat, passHatK(t.C, t.N, k))
		}
		st.Curve = append(st.Curve, kPoint{K: k, PassAtK: meanOf(at), PassHat: meanOf(hat)})
	}
	last := st.Curve[len(st.Curve)-1]
	st.PassAtK, st.PassHatK = last.PassAtK, last.PassHat
	return st
}

// lowerBound is the conservative lower confidence bound used by
// -gate-on lower-bound: the smaller of the clustered and Wilson bounds.
func (s evalStats) lowerBound() float64 {
	lb := s.Wilson.Lower
	if s.Clustered.Valid {
		lb = math.Min(lb, s.Clustered.Lower)
	}
	return lb
}

// bootstrapMeanCI is a percentile bootstrap 95% CI of the mean of xs,
// resampling with replacement. The RNG is seeded, so output is deterministic.
func bootstrapMeanCI(xs []float64, resamples int, seed uint64) (lo, hi float64) {
	if len(xs) == 0 {
		return 0, 0
	}
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	means := make([]float64, resamples)
	for i := range means {
		var s float64
		for range xs {
			s += xs[rng.IntN(len(xs))]
		}
		means[i] = s / float64(len(xs))
	}
	slices.Sort(means)
	at := func(q float64) float64 {
		return means[min(int(q*float64(resamples)), resamples-1)]
	}
	return at(0.025), at(0.975)
}
