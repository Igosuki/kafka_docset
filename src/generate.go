package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"github.com/mxk/go-sqlite/sqlite3"
	"golang.org/x/net/html"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// This code doesn't use concurrency and channels, it's just a script
// To make it evolve, and since a documentation page is rarely too large, one could use the
// Node struct from net/html and use recursion routines to determine when the program is matching
// the end of an enclosing tag that triggers specific work (such as the ToC here).

type entryType int32

const (
	Guide entryType = iota
	Root
)

type docset struct {
	path      string
	entryType entryType
}

func newClient() *http.Client {
	client := &http.Client{}
	client.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		fmt.Fprintf(os.Stdout, "Got redirected, don't know how to follow %v\n", r.URL)
		return nil
	}
	return client
}

func main() {
	var version = flag.String("kafka-version", "0.8.2", "The kafka version")

	r, _ := regexp.Compile("\\.")
	var urlVersion = r.ReplaceAllString(*version, "")
	var baseUrl = "http://kafka.apache.org"
	var docDir = "kafka.docset/Contents/Resources/Documents/"
	var pages = []*docset{
		&docset{path: "documentation.html", entryType: Root},
	}
	var xhtmlServiceUrl = "http://www.it.uc3m.es/jaf/cgi-bin/html2xhtml.cgi"

	sql, connErr := sqlite3.Open("kafka.docset/Contents/Resources/docSet.dsidx")

	// Always retry until timeout, tests showed locks are rarely over a millisecond
	busyFunc := func(count int) bool {
		return true
	}
	sql.BusyFunc(busyFunc)
	sql.BusyTimeout(1 * time.Second)
	if connErr != nil {
		panic(fmt.Errorf("Couldn't open sqlite3 database "))
	}
	count := 0
	for _, page := range pages {
		pageUrl := strings.Join([]string{baseUrl, urlVersion, page.path}, "/")
		documentPath := strings.Join([]string{docDir, page.path}, "")
		document, err := os.Create(documentPath)
		if err != nil {
			panic(err)
		}

		fmt.Println("GET: HTTP/1.1 : ", pageUrl)
		client := newClient()
		htmlDoc, err := client.Get(pageUrl)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error getting page %v : %v", pageUrl, err.Error())
			continue
		}

		fmt.Println("POST: HTTP/1.1 : ", xhtmlServiceUrl)
		postClient := newClient()

		htmlDocBytes, _ := ioutil.ReadAll(htmlDoc.Body)
		xhtmlDoc, postErr := postClient.Post(xhtmlServiceUrl, "text/html", bytes.NewReader(htmlDocBytes))
		if postErr != nil {
			fmt.Fprintf(os.Stderr, "Error calling xhtml service for ", page.path, postErr.Error())
			continue
		}

		writeAndFlush(document, xhtmlDoc.Body)
		xhtmlDoc.Body.Close()

		tokenizerReader, _ := os.Open(documentPath)
		tokenizer := html.NewTokenizer(tokenizerReader)
		tokenizer.AllowCDATA(true)

		tocDepth := 0
		tocMenuDepth := 0
		depthLevel := 0
		searchIndexQuery := "INSERT INTO searchIndex VALUES (NULL, $name, $type, $path)"
		sql.Exec("DELETE FROM searchIndex")

		for {
			tokenType := tokenizer.Next()
			if tokenizer.Err() == io.EOF {
				break
			}
			if tokenType == html.ErrorToken {
				println("Failed at depth level : ", depthLevel)
				fmt.Fprintln(os.Stderr, "Invalid html", tokenizer.Err())
				break
			}

			token := tokenizer.Token()
			switch tokenType {
			case html.TextToken:
				break
			case html.StartTagToken:
				tagName := token.Data
				if tagName == "link" {
					attrs := make(map[string]string)
					for _, attr := range token.Attr {
						attrs[attr.Key] = attr.Val
					}
					var href string
					if attrs["rel"] == "stylesheet" {
						if attrs["href"][0:4] == "http" {
							href = attrs["href"]
						} else {
							href = strings.Join([]string{baseUrl, attrs["href"][1:]}, "/")
						}
						sheetUrl, urlErr := url.Parse(href)
						if urlErr == nil {
							sheet, dlErr := client.Get(sheetUrl.String())
							fmt.Println("GET: HTTP/1.1 : ", href)
							sheetName := sheetUrl.Path[strings.LastIndex(sheetUrl.Path, "/"):]
							sheetFile, err := os.Create(strings.Join([]string{docDir, sheetName}, ""))
							if dlErr == nil && err == nil {
								writeAndFlush(sheetFile, sheet.Body)
							}
						}
					}
				}
				if tocDepth == 0 {
					for _, attr := range token.Attr {
						if attr.Key == "class" && strings.Contains(attr.Val, "toc") {
							fmt.Println("Found TOC at line ", depthLevel)
							tocDepth = depthLevel
						}
					}
				} else {
					switch tagName {
					case "a":
						for _, attr := range token.Attr {
							if attr.Key == "href" {
								var recordType string
								switch tocMenuDepth {
								case 0:
									recordType = "Guide"
								case 1:
									recordType = "Section"
								default:
									recordType = "Module"
								}

								tokenizer.Next()
								args := sqlite3.NamedArgs{"$name": tokenizer.Token().String(), "$type": recordType, "$path": strings.Join([]string{page.path, attr.Val}, "")}
								sqlErr := sql.Exec(searchIndexQuery, args)
								if sqlErr != nil {
									fmt.Println("SQL Error : ", sqlErr.Error())
								}
							}
						}
						break
					case "ul":
						tocMenuDepth++
						break
					default:
						break
					}
				}
				depthLevel++
				break
			case html.EndTagToken:
				count++
				depthLevel--
				fmt.Printf("DepthLevel %v, ToC Depth %v, ToC Menu Depth %v %v %s ", depthLevel, tocDepth, tocMenuDepth, count, "\n")
				if depthLevel == tocDepth {
					return
				} else if tocDepth > 0 && token.Data == "ul" {
					tocMenuDepth--
				}
				break
			case html.DoctypeToken:
				break
			}
		}
	}
}

func writeAndFlush(dst *os.File, src io.Reader) {
	writer := bufio.NewWriter(dst)

	_, wErr := io.Copy(writer, src)
	if wErr != nil {
		panic(fmt.Errorf("Can't write to file %v : %v", dst.Name(), wErr.Error()))
	}
	writer.Flush()
}
