# Kubernetes: a heuristic edge that is demonstrably wrong

Run: `pivot --repo <kubernetes@b2ec8b6f> --dependency os/exec --exclude-tests --depth 1`
Full output: `10-kubernetes-pivot.txt`. Parse: 608s cold, 13,843 files classified,
11,762 test files excluded before classification.

```
46 invalidated, 78 at-risk, 12877 safe, 48 unverified, 794 unreached
Evidence: 52 CONFIRMED, 71 HEURISTIC, 49 UNVERIFIED
```

**More of the evidence in this report was inferred than proved.** 71 heuristic against
52 confirmed. Before evidence tiers all 172 edges rendered identically, and any of
them could have carried a P0 work item on its own.

## The edge

```
[HEURISTIC] evidence: USES_TYPE pkg/controller/garbagecollector/graph.go:142
            -> cmd/prune-junit-xml/prunexml.go (confidence 0.75, resolution name_only)
```

Checked against source rather than trusted:

```go
// pkg/controller/garbagecollector/graph.go:142
func (n *node) setOwners(owners []metav1.OwnerReference) {

// cmd/prune-junit-xml/prunexml.go:309
type owners struct {
```

The parser matched a **parameter named `owners`** to an **unrelated type named `owners`**
in a different binary. `graph.go` does not import `cmd/prune-junit-xml` at all --
`grep -c prune-junit-xml pkg/controller/garbagecollector/graph.go` returns 0.

The edge is false. It is also exactly the kind of edge the graph is honest about: it
recorded `resolution: name_only` and `confidence: 0.75` at the moment it created it.
The information was always there. PivotMap threw it away.

Under the previous behaviour this line was indistinguishable from

```
IMPORTS internal/gitutil/git.go -> os/exec (confidence 0.80)
```

which is an import statement physically present in the file.

## The other half: 48 files that were reported SAFE

```
- build/common.sh                                     [PRODUCTION] parser reported E_PARSE_ERROR
- cluster/gce/config-default.sh                       [PRODUCTION] parser reported E_PARSE_ERROR
- cluster/addons/fluentd-gcp/fluentd-gcp-ds.yaml      [PRODUCTION] parser reported E_PARSE_ERROR
```

Shell and YAML the parser could not read. "No dependency path found" in a file that was
never successfully parsed is a statement about the parser, not about the file. These are
now UNVERIFIED, and the plan's execution contract forbids counting them towards the
constraint being satisfied.

## Why this repository

The card names dynamic dispatch, generated code and reflection. Kubernetes at this commit
carries all three at scale, counted rather than assumed:

```
4,259 files with a DO NOT EDIT header
1,592 files importing reflect
3,754 interface declarations
17,838 Go files
```

## Honest limits of this run

- One snapshot, one commit, one constraint. The tiers describe how each edge was
  resolved, not whether the resulting verdict is correct.
- `os/exec` in Kubernetes is a plausible constraint but not a real one anybody imposed.
- The 794 unreached files are inventory-only languages, which is the pre-existing
  language-level tier and not part of this change.
- CONFIRMED means the parser resolved the edge structurally. It does not mean the
  verdict built on it is right; interface dispatch still leaves no edge at all, so a
  file with genuinely no edges is still reported SAFE.
