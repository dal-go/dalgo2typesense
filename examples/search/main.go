// Command search demonstrates a bounded native Typesense search and a
// structured DALgo query against one existing collection.
//
// It is intentionally a manual example, not a test: set TYPESENSE_ENDPOINT,
// TYPESENSE_API_KEY and TYPESENSE_COLLECTION, then run it.
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo2typesense"
)

func main() {
	ctx := context.Background()
	db, err := dalgo2typesense.NewDB(ctx, dalgo2typesense.Config{
		Endpoint:   os.Getenv("TYPESENSE_ENDPOINT"),
		Collection: os.Getenv("TYPESENSE_COLLECTION"),
		Headers: func(context.Context) (map[string]string, error) {
			return map[string]string{"X-TYPESENSE-API-KEY": os.Getenv("TYPESENSE_API_KEY")}, nil
		},
	})
	if err != nil {
		log.Fatal(err)
	}

	searcher, ok := dal.As[dalgo2typesense.NativeSearcher](db)
	if !ok {
		log.Fatal("native search capability is missing")
	}
	fmt.Println("native search:")
	printResult(searcher, ctx)

	fmt.Println("structured DALgo query:")
	query := dal.From(dal.NewRootCollectionRef(os.Getenv("TYPESENSE_COLLECTION"), "")).
		NewQuery().
		Limit(10).
		SelectIntoRecord(nil)
	reader, err := db.ExecuteQueryToRecordsReader(ctx, query)
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	for {
		rec, err := reader.Next()
		if err != nil {
			break
		}
		fmt.Println(rec.Key().ID)
	}
}

func printResult(searcher dalgo2typesense.NativeSearcher, ctx context.Context) {
	result, err := searcher.Search(ctx, dalgo2typesense.SearchRequest{
		Q:        "*",
		QueryBy:  nil,
		Limit:    10,
		FilterBy: "",
	})
	if err != nil {
		log.Fatal(err)
	}
	for _, hit := range result.Hits {
		fmt.Println(hit.ID, hit.Document["title"])
	}
	fmt.Printf("found=%d out_of=%d returned=%d complete=%v partial=%v\n",
		result.Found, result.OutOf, result.Returned, result.Complete, result.Partial)
}
