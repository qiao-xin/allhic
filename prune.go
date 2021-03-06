/**
 * Filename: /Users/bao/code/allhic/prune.go
 * Path: /Users/bao/code/allhic
 * Created Date: Wednesday, February 28th 2018, 8:46:25 pm
 * Author: bao
 *
 * Copyright (c) 2018 Haibao Tang
 */

package allhic

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

// Pruner processes the pruning step
type Pruner struct {
	AllelesFile  string
	PairsFile    string
	edges        []ContigPair
	alleleGroups []AlleleGroup
}

// AlleleGroup stores the contig names that are considered allelic
type AlleleGroup []string

// CtgAlleleGroupPair stores a pair of the contig and the alleleGroup it resides in
type CtgAlleleGroupPair struct {
	ctg     string
	groupID int
}

// Run calls the pruning steps
// The pruning algorithm is a heuristic method that removes the following pairs:
//
// 1. Allelic, these are directly the pairs of allelic contigs given in the allele table
// 2. Cross-allelic, these are any contigs that connect to the allelic contigs so we only
//    keep the best contig pair
//
// Pruned edges are then annotated as allelic/cross-allelic/ok
func (r *Pruner) Run() {
	r.edges = parseDist(r.PairsFile)
	r.alleleGroups = parseAllelesTable(r.AllelesFile)
	r.pruneAllelic()
	r.pruneCrossAllelic()
	newPairsFile := RemoveExt(r.PairsFile) + ".prune.txt"
	writePairsFile(newPairsFile, r.edges)
}

// pruneAllelic removes the allelic contigs given in the allele table
// we iterate through all the allele groups and mark the pairs that are considered allelic
func (r *Pruner) pruneAllelic() {
	// Find all blacklisted allelic pairs
	allelicPairs := map[[2]string]bool{}
	for _, alleleGroup := range r.alleleGroups {
		for i := 0; i < len(alleleGroup); i++ {
			for j := i + 1; j < len(alleleGroup); j++ {
				a, b := alleleGroup[i], alleleGroup[j]
				if a > b {
					b, a = a, b
				}
				allelicPairs[[2]string{a, b}] = true
			}
		}
	}

	// Now iterate over all edges and mark
	pruned, prunedLinks := 0, 0
	total, totalLinks := 0, 0
	for i, edge := range r.edges {
		pair := [2]string{edge.at, edge.bt}
		if _, ok := allelicPairs[pair]; ok {
			r.edges[i].label = "allelic"
			pruned++
			prunedLinks += edge.nObservedLinks
		}
		total++
		totalLinks += edge.nObservedLinks
	}
	log.Noticef("Allelic pairs imported: %d, pruned: %s, prunedLinks: %s",
		len(allelicPairs), Percentage(pruned, total), Percentage(prunedLinks, totalLinks))
}

// pruneCrossAllelic removes contigs that link to multiple allelic contigs and we choose
// to keep a single best link, i.e. for each allele group, we find the best link to each
// contig and retain. Note that since contigs may be in different allele groups, the order
// of removal may affect end results.
func (r *Pruner) pruneCrossAllelic() {
	// Store contig to list of alleleGroups since each contig can be in different alleleGroups
	ctgToAlleleGroup := map[string][]int{}
	for groupID, alleleGroup := range r.alleleGroups {
		for _, ctg := range alleleGroup {
			if gg, ok := ctgToAlleleGroup[ctg]; ok {
				gg = append(gg, groupID)
			} else {
				ctgToAlleleGroup[ctg] = []int{groupID}
			}
		}
	}

	// Store the best match of each contig to an allele group
	scores := map[CtgAlleleGroupPair]int{} // (ctg, alleleGroupID) => score
	for _, edge := range r.edges {
		if edge.label != "ok" { // We skip the allelic pairs since these are already removed
			continue
		}
		updateScore(edge.at, edge.bt, edge.nObservedLinks, ctgToAlleleGroup, scores)
		updateScore(edge.bt, edge.at, edge.nObservedLinks, ctgToAlleleGroup, scores)
	}

	// Now iterate over all edges and mark
	pruned, prunedLinks := 0, 0
	total, totalLinks := 0, 0
	for i, edge := range r.edges {
		aBestScore := getScore(edge.at, edge.bt, ctgToAlleleGroup, scores)
		bBestScore := getScore(edge.bt, edge.at, ctgToAlleleGroup, scores)
		if edge.nObservedLinks < aBestScore || edge.nObservedLinks < bBestScore {
			r.edges[i].label = fmt.Sprintf("cross-allelic(%d|%d)", aBestScore, bBestScore)
			pruned++
			prunedLinks += edge.nObservedLinks
		}
		total++
		totalLinks += edge.nObservedLinks
	}
	log.Noticef("Cross-allelic pairs pruned: %s, prunedLinks: %s",
		Percentage(pruned, total), Percentage(prunedLinks, totalLinks))
}

// updateScore takes a potential pair of contigs and update scores
func updateScore(at, bt string, score int, ctgToAlleleGroup map[string][]int, scores map[CtgAlleleGroupPair]int) {
	if gg, ok := ctgToAlleleGroup[bt]; ok {
		// Update through all alleleGroups that contig b sits in
		for _, bg := range gg {
			pair := CtgAlleleGroupPair{at, bg}
			// TODO: the score should ideally be 'normalized' score, since contig size can affect size, and as a
			//       result, the "best match" in absolute score may be wrong
			if sc, ok := scores[pair]; ok {
				if sc < score {
					scores[pair] = score
				}
			} else {
				scores[pair] = score
			}
		}
	}
}

// getScore takes a pair of contigs and get the maximum score to the allele group
func getScore(at, bt string, ctgToAlleleGroup map[string][]int, scores map[CtgAlleleGroupPair]int) int {
	if gg, ok := ctgToAlleleGroup[bt]; ok {
		maxScore := -1
		for _, bg := range gg {
			maxScoreAlleleGroup, _ := scores[CtgAlleleGroupPair{at, bg}]
			if maxScore < maxScoreAlleleGroup {
				maxScore = maxScoreAlleleGroup
			}
		}
		return maxScore // maximum score among all allele group matchings
	}
	return -1
}

// parseAllelesTable imports the contig allelic relationship
// File is a tab-separated file that looks like the following:
// Chr10   18902   tig00030660     tig00003333
// Chr10   35071   tig00038687     tig00038686     tig00065419
func parseAllelesTable(allelesFile string) []AlleleGroup {
	log.Noticef("Parse alleles table `%s`", allelesFile)
	fh := mustOpen(allelesFile)
	defer fh.Close()

	reader := bufio.NewReader(fh)

	var data []AlleleGroup
	for {
		row, err := reader.ReadString('\n')
		row = strings.TrimSpace(row)
		if row == "" && err == io.EOF {
			break
		}
		if err != nil {
			log.Fatal(err)
		}
		words := strings.Split(row, "\t")
		if len(words) <= 3 { // Must have at least 4 fields, i.e. 1 pair
			continue
		}
		alleleGroup := make(AlleleGroup, len(words)-2)
		copy(alleleGroup, words[2:])
		data = append(data, alleleGroup)
	}

	return data
}

// writePairsFile simply writes pruned contig pairs to file
func writePairsFile(pairsFile string, edges []ContigPair) {
	f, _ := os.Create(pairsFile)
	w := bufio.NewWriter(f)
	defer f.Close()
	fmt.Fprintf(w, PairsFileHeader)

	for _, c := range edges {
		fmt.Fprintln(w, c)
	}
	w.Flush()
	log.Noticef("Pruned contig pair analyses written to `%s`", pairsFile)
}
