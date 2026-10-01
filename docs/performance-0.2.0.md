# 0.2.0 context compaction evidence

Measured 2026-10-01 on Apple M2, Darwin arm64, Go 1.26.6. Each of the six
unchanged `BenchmarkProviderReplayCompaction` fixtures ran five times with
`-cpu=1 -benchtime=200ms -benchmem`. Baseline provider source is from `76477ea`.
The runs were sequential on the same host, but the machine was not dedicated;
wall-clock differences, especially small ones, are not statistical guarantees.

## Implementation and contract

Compaction now marshals each item once to measure its exact JSON byte length,
including escaping. The array budget adds brackets and commas (and distinguishes
nil `null` from empty `[]`). Each replacement adjusts the total by that item's
encoded size difference. This removes whole-array encoding after every old tool
output, changing the compaction work from quadratic to linear in the encoded
input plus replacements.

The oldest-first policy is unchanged, including replacement of a short output
with a longer omission marker. Order, non-output items and newer output bodies
remain intact. Replacements now preserve IDs, status, caller and SDK extension
metadata instead of reconstructing a minimal output item. Caller-owned input
is never mutated. SDK raw/extra-field overrides are handled using their actual
wire representation, so they cannot override the compacted body.

## Observed medians

`MB/op` below uses decimal megabytes of cumulative allocation, not peak resident
memory. The unbounded path allocates 2–4% more bytes because of per-item JSON
measurement; this is an explicit tradeoff for bounded linear compaction.

| Outputs | Compaction | ms/op before → after | MB/op before → after | allocs/op before → after |
| --- | --- | --- | --- | --- |
| 16 | false | 0.336 → 0.352 | 0.403 → 0.412 | 256 → 289 |
| 16 | true | 2.947 → 0.371 | 3.371 → 0.436 | 3568 → 524 |
| 64 | false | 1.337 → 1.348 | 1.583 → 1.643 | 980 → 1109 |
| 64 | true | 46.758 → 1.465 | 49.496 → 1.736 | 49841 → 2010 |
| 256 | false | 6.298 → 5.431 | 6.314 → 6.571 | 3867 → 4389 |
| 256 | true | 769.087 → 5.999 | 776.473 → 6.942 | 776167 → 7992 |

No live model, network latency, API quota or task-solving score is represented by
these results. Five samples do not establish service latency percentiles. The
large bounded-path reduction is supported by both timing and allocation data;
small unbounded-path timing changes should be treated as noise.

## Verification and reproduction

- `go test -race ./internal/provider` passed.
- `go test ./internal/provider -run '^$' -fuzz '^FuzzBoundCustomTurnInput$' -fuzztime=10s -parallel=1`
  passed 31,176 executions; fuzz generation is nondeterministic.
- Deterministic tests cover exact and one-byte budget boundaries, escape-heavy
  Unicode/invalid UTF-8 input, nil and empty contexts, irreducible input, short
  outputs, stable repeated checks, immutable input, and optional/extension
  protocol metadata. A deliberately slow whole-array oracle checks fuzz inputs.
- The existing strict report comparator accepted identical benchmark/metric
  sets and matching Go, OS, CPU, CPU limit and benchtime.

Reproduce the focused measurement on each revision, sequentially:

```sh
go test -json -count=5 -cpu=1 -timeout=10m -run='^$' \
  -bench='^BenchmarkProviderReplayCompaction$' -benchmem -benchtime=200ms \
  ./internal/provider > compaction.log
```

The baseline used the existing report runner's `./...` package selection; the
candidate selected `./internal/provider`. Both run the same provider benchmark
executable and unchanged fixtures. No other package matches this filter. This
focused result does not claim that whole-repository checks have passed.

Local reports and raw logs are retained under ignored `benchmarks/results/`:

- `compaction-0.2.0-before.json` and `.log`
- `compaction-0.2.0-provider-after.json` and `.log`
- `compaction-0.2.0-comparison.json`

For durable review evidence, the raw measurement samples are included below.
Reproduce them rather than using these local numbers as a CI timing gate.

## Raw samples and source identity

Unchanged benchmark fixture SHA-256: `8d9df27c0ca1d8e791bd75b414d1b8ef4df0ec4f66e8b575a124ccb1f9a4964c`.

Provider source SHA-256 before: `0e1db62ded67b7325825af66019832d7c93995f8e90d044ebf1a73966286ed02`.

Provider source SHA-256 after: `940fef06660583e0e1744d310e36a79c9c699826ea95ad1df5856654fbb752a4`.

```json
{
  "outputs_16/compact_false": {
    "ns/op": {
      "before": [
        376162.0,
        337362.0,
        335396.0,
        336173.0,
        335827.0
      ],
      "after": [
        372330.0,
        359762.0,
        345452.0,
        336439.0,
        352349.0
      ]
    },
    "B/op": {
      "before": [
        403089.0,
        402754.0,
        402754.0,
        402746.0,
        402752.0
      ],
      "after": [
        411983.0,
        411911.0,
        411915.0,
        411911.0,
        411909.0
      ]
    },
    "allocs/op": {
      "before": [
        256.0,
        256.0,
        256.0,
        256.0,
        256.0
      ],
      "after": [
        289.0,
        289.0,
        289.0,
        289.0,
        289.0
      ]
    }
  },
  "outputs_16/compact_true": {
    "ns/op": {
      "before": [
        2946580.0,
        2946740.0,
        2933220.0,
        2936456.0,
        2956375.0
      ],
      "after": [
        370823.0,
        363347.0,
        367967.0,
        374462.0,
        378229.0
      ]
    },
    "B/op": {
      "before": [
        3371119.0,
        3371120.0,
        3371181.0,
        3371119.0,
        3371120.0
      ],
      "after": [
        436122.0,
        436120.0,
        436124.0,
        436125.0,
        436120.0
      ]
    },
    "allocs/op": {
      "before": [
        3568.0,
        3568.0,
        3568.0,
        3568.0,
        3568.0
      ],
      "after": [
        524.0,
        524.0,
        524.0,
        524.0,
        524.0
      ]
    }
  },
  "outputs_256/compact_false": {
    "ns/op": {
      "before": [
        5934817.0,
        6640016.0,
        6057991.0,
        6297701.0,
        7507343.0
      ],
      "after": [
        5428223.0,
        5430902.0,
        5417318.0,
        5618374.0,
        5592868.0
      ]
    },
    "B/op": {
      "before": [
        6367927.0,
        6314017.0,
        6313405.0,
        6313738.0,
        6314013.0
      ],
      "after": [
        6570702.0,
        6570688.0,
        6570659.0,
        6570677.0,
        6570674.0
      ]
    },
    "allocs/op": {
      "before": [
        3866.0,
        3867.0,
        3867.0,
        3867.0,
        3867.0
      ],
      "after": [
        4390.0,
        4390.0,
        4389.0,
        4389.0,
        4389.0
      ]
    }
  },
  "outputs_256/compact_true": {
    "ns/op": {
      "before": [
        1069082167.0,
        741497750.0,
        888745750.0,
        769087375.0,
        684185083.0
      ],
      "after": [
        6097366.0,
        6106413.0,
        5999126.0,
        5960387.0,
        5851094.0
      ]
    },
    "B/op": {
      "before": [
        776474376.0,
        776472232.0,
        776474672.0,
        776473304.0,
        776473464.0
      ],
      "after": [
        6942159.0,
        6942159.0,
        6942144.0,
        6942159.0,
        6942098.0
      ]
    },
    "allocs/op": {
      "before": [
        776179.0,
        776151.0,
        776183.0,
        776165.0,
        776167.0
      ],
      "after": [
        7992.0,
        7992.0,
        7992.0,
        7992.0,
        7992.0
      ]
    }
  },
  "outputs_64/compact_false": {
    "ns/op": {
      "before": [
        1322774.0,
        1344743.0,
        1331181.0,
        1337452.0,
        1459896.0
      ],
      "after": [
        1354766.0,
        1348210.0,
        1337641.0,
        1325175.0,
        1354809.0
      ]
    },
    "B/op": {
      "before": [
        1585697.0,
        1582967.0,
        1582968.0,
        1582967.0,
        1582970.0
      ],
      "after": [
        1643229.0,
        1643227.0,
        1643227.0,
        1643369.0,
        1643240.0
      ]
    },
    "allocs/op": {
      "before": [
        980.0,
        980.0,
        980.0,
        980.0,
        980.0
      ],
      "after": [
        1109.0,
        1109.0,
        1109.0,
        1109.0,
        1109.0
      ]
    }
  },
  "outputs_64/compact_true": {
    "ns/op": {
      "before": [
        91644194.0,
        46758000.0,
        44758208.0,
        44601467.0,
        52446479.0
      ],
      "after": [
        1464625.0,
        1466585.0,
        1471804.0,
        1461975.0,
        1463551.0
      ]
    },
    "B/op": {
      "before": [
        49495736.0,
        49630772.0,
        49495723.0,
        49500720.0,
        49495788.0
      ],
      "after": [
        1736083.0,
        1736100.0,
        1736083.0,
        1736085.0,
        1736086.0
      ]
    },
    "allocs/op": {
      "before": [
        49841.0,
        49848.0,
        49841.0,
        49844.0,
        49841.0
      ],
      "after": [
        2010.0,
        2010.0,
        2010.0,
        2010.0,
        2010.0
      ]
    }
  }
}
```
