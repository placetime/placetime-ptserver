package main

import (
	"cgl.tideland.biz/applog"
	"crypto/md5"
	"fmt"
	"github.com/iand/eventful"
	"github.com/iand/lastfm"
	"github.com/iand/spotify"
	"github.com/iand/youtube"
	"github.com/placetime/datastore"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path"
	"regexp"
	"time"
)

type SearchResults struct {
	Results interface{} `json:"results"`
}

type ProfileSearchResults []*datastore.Profile

type ItemSearchResults []*datastore.Item
type FormattedItemSearchResults []*datastore.FormattedItem

type SearchFunc func(srch string) ItemSearchResults

func ProfileSearch(srch string) SearchResults {
	s := datastore.NewRedisStore()
	defer s.Close()

	plist, _ := s.FindProfilesBySubstring(srch)

	return SearchResults{Results: plist}
}

func ItemSearch(srch string, pid datastore.PidType) SearchResults {
	searches := []SearchFunc{
		searchYoutubeVidoes,
		searchEventfulEvents,
		searchSpotifyTracks,
	}

	return MultiplexedSearch(srch, searches)
}

func VideoSearch(srch string, pid datastore.PidType) SearchResults {

	searches := []SearchFunc{
		searchYoutubeVidoes,
	}

	return MultiplexedSearch(srch, searches)
}

func AudioSearch(srch string, pid datastore.PidType) SearchResults {

	searches := []SearchFunc{
		searchSpotifyTracks,
	}

	return MultiplexedSearch(srch, searches)
}

func EventSearch(srch string, pid datastore.PidType) SearchResults {

	searches := []SearchFunc{
		searchEventfulEvents,
	}

	return MultiplexedSearch(srch, searches)
}

func MultiplexedSearch(srch string, searches []SearchFunc) SearchResults {
	results := make(ItemSearchResults, 0)

	items := make(chan ItemSearchResults)

	for _, f := range searches {
		go func() { items <- f(srch) }()
	}

	lists := make([]ItemSearchResults, 0)

	timeout := time.After(time.Duration(config.Search.Timeout) * time.Millisecond)
	for i := 0; i < len(searches); i++ {
		select {
		case result := <-items:
			lists = append(lists, result)
		case <-timeout:
			applog.Debugf("Search timed out")
			break
		}
	}

	i := 0
	added := true
	for added {
		added = false
		for _, list := range lists {
			if i < len(list) {
				results = append(results, list[i])
				added = true
			}
		}
		i++
	}

	return SearchResults{Results: results}

}

func searchYoutubeVidoes(srch string) ItemSearchResults {
	items := make([]*datastore.Item, 0)

	c := youtube.New()

	feed, err := c.VideoSearch(srch)
	if err != nil {
		applog.Errorf("Fetch of feed got http error  %s", err.Error())
		return items
	}

	if feed != nil {
		for _, item := range feed.Entries {
			hasher := md5.New()
			io.WriteString(hasher, item.ID.Value)
			id := datastore.ItemIdType(fmt.Sprintf("%x", hasher.Sum(nil)))

			var url string
			for _, link := range item.Links {
				if link.Rel == "self" {
					url = link.Href
					break
				}
			}

			bestImage, bestImageName := "", ""

			for _, img := range item.Media.Thumbnails {
				if img.Name == "sddefault" ||
					(img.Name == "hqdefault" && bestImageName != "sddefault") ||
					(img.Name == "mqdefault" && bestImageName != "sddefault" && bestImageName != "hqdefault") ||
					(img.Name == "default " && bestImageName != "mqdefault" && bestImageName != "sddefault" && bestImageName != "hqdefault") {
					bestImage = img.URL
					bestImageName = img.Name

				}
			}

			items = append(items, &datastore.Item{Id: id, Pid: datastore.PidType(config.Search.Youtube.Pid), Event: 0, Text: item.Title.Value, Link: url, Media: "video", Image: bestImage, Duration: item.Media.Duration.Seconds})
		}
	}
	return items

}

func searchEventfulEvents(srch string) ItemSearchResults {
	items := make([]*datastore.Item, 0)

	c := eventful.New(config.Search.Eventful.AppKey)

	results, err := c.SearchEvents(srch, "Future")
	if err != nil {
		applog.Errorf("Fetch of events got error  %s", err.Error())
		return items
	}

	applog.Debugf("Received %d items from eventful matching %s", len(results.Events), srch)
	for _, event := range results.Events {
		hasher := md5.New()
		io.WriteString(hasher, event.ID)
		id := datastore.ItemIdType(fmt.Sprintf("%x", hasher.Sum(nil)))

		imgURL := ""
		if event.Image != nil && event.Image.Medium != nil {
			imgURL = event.Image.Medium.URL
		}

		duration := 0
		startTime, err := time.Parse("2006-01-02 15:04:05", event.StartTime)
		if err != nil {
			startTime = time.Unix(0, 0)
		} else {
			stopTime, err := time.Parse("2006-01-02 15:04:05", event.StopTime)
			if err == nil {
				duration = int(stopTime.Sub(startTime).Seconds())
			}

		}

		items = append(items, &datastore.Item{Id: id, Pid: datastore.PidType(config.Search.Eventful.Pid), Event: datastore.FakeEventPrecision(startTime), Text: event.Title, Link: event.URL, Media: "event", Image: imgURL, Duration: duration})
	}
	return items

}

func searchSpotifyTracks(srch string) ItemSearchResults {
	items := make([]*datastore.Item, 0)

	client := spotify.New()
	resp, err := client.SearchTracks(srch, 1)

	if err != nil {
		applog.Errorf("Fetch of spotify search got http error  %s", err.Error())
		return items
	}

	count := 0
	if resp != nil {
		applog.Debugf("Received %d items from spotify matching %s", len(resp.Tracks), srch)
		for _, track := range resp.Tracks {
			if len(track.Artists) > 0 {
				hasher := md5.New()
				io.WriteString(hasher, track.URI)
				id := datastore.ItemIdType(fmt.Sprintf("%x", hasher.Sum(nil)))

				artist := track.Artists[0].Name

				var imgPath string
				imgPath = fetchTrackImage(track.URI)

				text := fmt.Sprintf("%s / %s", track.Name, artist)

				items = append(items, &datastore.Item{
					Id:       id,
					Pid:      datastore.PidType(config.Search.Spotify.Pid),
					Event:    0,
					Text:     text,
					Link:     track.URI,
					Media:    "audio",
					Image:    imgPath,
					Duration: int(track.Length),
				})

				count++
				if count > 15 {
					break
				}
			}
		}
	}
	return items

}

// spotify:track:24H5KPBdSvHQMRXTp12K3J
// http://open.spotify.com/track/24H5KPBdSvHQMRXTp12K3J

func fetchTrackImage(spotifyURL string) string {
	if len(spotifyURL) < 36 {
		return ""
	}
	hash := spotifyURL[14:]

	pageUrl := fmt.Sprintf("http://open.spotify.com/track/%s", hash)
	// applog.Debugf("Fetching spotify page %s", pageUrl)

	resp, err := http.Get(pageUrl)
	if err != nil {
		applog.Errorf("Fetch of spotify page %s got http error %s", pageUrl, err.Error())
		return ""
	}

	defer resp.Body.Close()

	content, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		applog.Errorf("Read of spotify page %s got io error %s", pageUrl, err.Error())
		return ""
	}

	re, err := regexp.Compile(`"(http://o\.scdn\.co/300/[A-Za-z0-9]+)"`)
	if err != nil {
		return ""
	}

	matches := re.FindAllSubmatch(content, -1)
	if len(matches) > 0 {
		return string(matches[0][1])
	}

	return ""
}

func fetchTrackImageLastfm(trackname string, artist string, itemID datastore.ItemIdType) (string, error) {
	filename := fmt.Sprintf("%s.png", itemID)
	foutName := path.Join(config.Image.Path, filename)

	if _, err := os.Stat(foutName); err == nil {
		return filename, nil
	}

	lc := lastfm.New(config.Search.Lastfm.APIKey)
	track, err := lc.TrackInfoByName(trackname, artist, "")
	if err != nil {
		return "", err
	}

	bestImageURL := ""
	bestImageSize := ""
	for _, img := range track.Album.Image {

		if img.Size == "mega" {
			bestImageURL = img.URL
			break
		}

		if img.Size == "extralarge" ||
			(img.Size == "large" && bestImageSize != "extralarge") ||
			(img.Size == "medium" && bestImageSize != "extralarge" && bestImageSize != "large") ||
			(img.Size == "small" && bestImageSize != "extralarge" && bestImageSize != "large" && bestImageSize != "medium") {
			bestImageSize = img.Size
			bestImageURL = img.URL
		}

	}

	return bestImageURL, nil
}
