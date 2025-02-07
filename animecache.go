package anidb

import (
	"fmt"
	"github.com/EliasFleckenstein03/go-anidb/http"
	"github.com/EliasFleckenstein03/go-anidb/misc"
	"github.com/EliasFleckenstein03/go-anidb/udp"
	"github.com/EliasFleckenstein03/go-fscache"
	"sort"
	"strconv"
	"strings"
	"time"
)

var _ cacheable = &Anime{}

func (a *Anime) setCachedTS(ts time.Time) {
	a.Cached = ts
}

func (a *Anime) IsStale() bool {
	if a == nil {
		return true
	}
	now := time.Now()
	diff := now.Sub(a.Cached)
	if a.Incomplete {
		return diff > AnimeIncompleteCacheDuration
	}

	// If the anime ended, and more than AnimeCacheDuration time ago at that
	if !a.EndDate.IsZero() && now.After(a.EndDate.Add(AnimeCacheDuration)) {
		return diff > FinishedAnimeCacheDuration
	}
	return diff > AnimeCacheDuration
}

// Unique Anime IDentifier.
type AID int

// Returns a cached Anime. Returns nil if there is no cached Anime with this AID.
func (aid AID) Anime() *Anime {
	var a Anime
	if CacheGet(&a, "aid", aid) == nil {
		return &a
	}
	return nil
}

type httpAnimeResponse struct {
	anime httpapi.Anime
	err   error
}

// Retrieves an Anime by its AID. Uses both the HTTP and UDP APIs,
// but can work without the UDP API.
func (adb *AniDB) AnimeByID(aid AID) <-chan *Anime {
	key := []fscache.CacheKey{"aid", aid}
	ch := make(chan *Anime, 1)

	if aid < 1 {
		ch <- nil
		close(ch)
	}

	ic := make(chan notification, 1)
	go func() { ch <- (<-ic).(*Anime); close(ch) }()
	if intentMap.Intent(ic, key...) {
		return ch
	}

	if !Cache.IsValid(InvalidKeyCacheDuration, key...) {
		intentMap.NotifyClose((*Anime)(nil), key...)
		return ch
	}

	anime := aid.Anime()
	if !anime.IsStale() {
		intentMap.NotifyClose(anime, key...)
		return ch
	}

	go func() {
		httpChan := make(chan httpAnimeResponse, 1)
		go func() {
			adb.Logger.Printf("HTTP>>> Anime %d", aid)
			a, err := httpapi.GetAnime(int(aid))
			httpChan <- httpAnimeResponse{anime: a, err: err}
		}()
		udpChan := adb.udp.SendRecv("ANIME",
			paramMap{
				"aid":   aid,
				"amask": animeAMask,
			})

		timeout := time.After(adb.Timeout)

		if anime == nil {
			anime = &Anime{AID: aid}
		}
		anime.Incomplete = true

		ok := true

	Loop:
		for i := 0; i < 2; i++ {
			select {
			case <-timeout:
				// HTTP API timeout
				if httpChan != nil {
					adb.Logger.Printf("HTTP<<< Timeout")
					close(httpChan)
				}
			case resp := <-httpChan:
				if resp.err != nil {
					adb.Logger.Printf("HTTP<<< %v", resp.err)
					ok = false
					break Loop
				}

				if resp.anime.Error != "" {
					adb.Logger.Printf("HTTP<<< Error %q", resp.anime.Error)
				}

				if anime.populateFromHTTP(resp.anime) {
					adb.Logger.Printf("HTTP<<< Anime %q", anime.PrimaryTitle)
				} else {
					// HTTP ok but parsing not ok
					if anime.PrimaryTitle == "" {
						Cache.SetInvalid(key...)
					}

					switch resp.anime.Error {
					case "Anime not found", "aid Missing or Invalid":
						// deleted AID?
						Cache.Delete(key...)
					}

					ok = false
					break Loop
				}

				httpChan = nil
			case reply := <-udpChan:
				if reply.Code() == 330 {
					Cache.SetInvalid(key...)
					// deleted AID?
					Cache.Delete(key...)

					ok = false
					break Loop
				} else {
					anime.Incomplete = !anime.populateFromUDP(reply)
				}
				udpChan = nil
			}
		}
		if anime.PrimaryTitle != "" {
			if ok {
				CacheSet(anime, key...)
			}
			intentMap.NotifyClose(anime, key...)
		} else {
			intentMap.NotifyClose((*Anime)(nil), key...)
		}
	}()
	return ch
}

func (a *Anime) populateFromHTTP(reply httpapi.Anime) bool {
	if reply.Error != "" {
		return false
	}

	if a.AID != AID(reply.ID) {
		panic(fmt.Sprintf("Requested AID %d different from received AID %d", a.AID, reply.ID))
	}
	a.R18 = reply.R18

	a.Type = AnimeType(reply.Type)
	// skip episode count since it's unreliable; UDP API handles that

	// UDP API has more precise versions
	if a.Incomplete {
		if st, err := time.Parse(httpapi.DateFormat, reply.StartDate); err == nil {
			a.StartDate = st
		}
		if et, err := time.Parse(httpapi.DateFormat, reply.EndDate); err == nil {
			a.EndDate = et
		}
	}

	for _, title := range reply.Titles {
		switch title.Type {
		case "main":
			a.PrimaryTitle = title.Title
		case "official":
			if a.OfficialTitles == nil {
				a.OfficialTitles = make(UniqueTitleMap)
			}
			a.OfficialTitles[Language(title.Lang)] = title.Title
		case "short":
			if a.ShortTitles == nil {
				a.ShortTitles = make(TitleMap)
			}
			a.ShortTitles[Language(title.Lang)] = append(a.ShortTitles[Language(title.Lang)], title.Title)
		case "synonym":
			if a.Synonyms == nil {
				a.Synonyms = make(TitleMap)
			}
			a.Synonyms[Language(title.Lang)] = append(a.Synonyms[Language(title.Lang)], title.Title)
		}
	}

	a.OfficialURL = reply.URL
	if reply.Picture != "" {
		a.Picture = httpapi.AniDBImageBaseURL + reply.Picture
	}

	a.Description = reply.Description

	a.Votes = Rating{
		Rating:    reply.Ratings.Permanent.Rating,
		VoteCount: reply.Ratings.Permanent.Count,
	}
	a.TemporaryVotes = Rating{
		Rating:    reply.Ratings.Temporary.Rating,
		VoteCount: reply.Ratings.Temporary.Count,
	}
	a.Reviews = Rating{
		Rating:    reply.Ratings.Review.Rating,
		VoteCount: reply.Ratings.Review.Count,
	}

	a.populateResources(reply.Resources)

	counts := map[misc.EpisodeType]int{}

	sort.Sort(reply.Episodes)
	for _, ep := range reply.Episodes {
		ad, _ := time.Parse(httpapi.DateFormat, ep.AirDate)

		titles := make(UniqueTitleMap)
		for _, title := range ep.Titles {
			titles[Language(title.Lang)] = title.Title
		}

		e := &Episode{
			EID: EID(ep.ID),
			AID: a.AID,

			Episode: *misc.ParseEpisode(ep.EpNo.EpNo),

			Length:  time.Duration(ep.Length) * time.Minute,
			AirDate: &ad,

			Rating: Rating{
				Rating:    ep.Rating.Rating,
				VoteCount: ep.Rating.Votes,
			},
			Titles: titles,
		}
		counts[e.Type]++
		cacheEpisode(e)

		a.Episodes = append(a.Episodes, e)
	}

	a.EpisodeCount = misc.EpisodeCount{
		RegularCount: counts[misc.EpisodeTypeRegular],
		SpecialCount: counts[misc.EpisodeTypeSpecial],
		CreditsCount: counts[misc.EpisodeTypeCredits],
		OtherCount:   counts[misc.EpisodeTypeOther],
		TrailerCount: counts[misc.EpisodeTypeTrailer],
		ParodyCount:  counts[misc.EpisodeTypeParody],
	}

	if a.Incomplete {
		if !a.EndDate.IsZero() {
			a.TotalEpisodes = a.EpisodeCount.RegularCount
		}
	}

	return true
}

func (a *Anime) populateResources(list []httpapi.Resource) {
	a.Resources.AniDB = Resource{fmt.Sprintf("http://anidb.net/a%v", a.AID)}

	for _, res := range list {
		args := make([][]interface{}, len(res.ExternalEntity))
		for i, e := range res.ExternalEntity {
			args[i] = make([]interface{}, len(e.Identifiers))
			for j := range args[i] {
				args[i][j] = e.Identifiers[j]
			}
		}

		switch res.Type {
		case 1: // ANN
			for i := range res.ExternalEntity {
				a.Resources.ANN =
					append(a.Resources.ANN, fmt.Sprintf(httpapi.ANNFormat, args[i]...))
			}
		case 2: // MyAnimeList
			for i := range res.ExternalEntity {
				a.Resources.MyAnimeList =
					append(a.Resources.MyAnimeList, fmt.Sprintf(httpapi.MyAnimeListFormat, args[i]...))
			}
		case 3: // AnimeNfo
			for i := range res.ExternalEntity {
				a.Resources.AnimeNfo =
					append(a.Resources.AnimeNfo, fmt.Sprintf(httpapi.AnimeNfoFormat, args[i]...))
			}
		case 4: // OfficialJapanese
			for _, e := range res.ExternalEntity {
				for _, url := range e.URL {
					a.Resources.OfficialJapanese = append(a.Resources.OfficialJapanese, url)
				}
			}
		case 5: // OfficialEnglish
			for _, e := range res.ExternalEntity {
				for _, url := range e.URL {
					a.Resources.OfficialEnglish = append(a.Resources.OfficialEnglish, url)
				}
			}
		case 6: // WikipediaEnglish
			for i := range res.ExternalEntity {
				a.Resources.WikipediaEnglish =
					append(a.Resources.WikipediaEnglish, fmt.Sprintf(httpapi.WikiEnglishFormat, args[i]...))
			}
		case 7: // WikipediaJapanese
			for i := range res.ExternalEntity {
				a.Resources.WikipediaJapanese =
					append(a.Resources.WikipediaJapanese, fmt.Sprintf(httpapi.WikiJapaneseFormat, args[i]...))
			}
		case 8: // SyoboiSchedule
			for i := range res.ExternalEntity {
				a.Resources.SyoboiSchedule =
					append(a.Resources.SyoboiSchedule, fmt.Sprintf(httpapi.SyoboiFormat, args[i]...))
			}
		case 9: // AllCinema
			for i := range res.ExternalEntity {
				a.Resources.AllCinema =
					append(a.Resources.AllCinema, fmt.Sprintf(httpapi.AllCinemaFormat, args[i]...))
			}
		case 10: // Anison
			for i := range res.ExternalEntity {
				a.Resources.Anison =
					append(a.Resources.Anison, fmt.Sprintf(httpapi.AnisonFormat, args[i]...))
			}
		case 14: // VNDB
			for i := range res.ExternalEntity {
				a.Resources.VNDB =
					append(a.Resources.VNDB, fmt.Sprintf(httpapi.VNDBFormat, args[i]...))
			}
		case 15: // MaruMegane
			for i := range res.ExternalEntity {
				a.Resources.MaruMegane =
					append(a.Resources.MaruMegane, fmt.Sprintf(httpapi.MaruMeganeFormat, args[i]...))
			}
		}
	}
}

// http://wiki.anidb.info/w/UDP_API_Definition#ANIME:_Retrieve_Anime_Data
// Everything that we can't easily get through the HTTP API, or that has more accuracy:
// episodes, air date, end date, award list, update date,
const animeAMask = "0000980201"

func (a *Anime) populateFromUDP(reply udpapi.APIReply) bool {
	if reply != nil && reply.Error() == nil {
		parts := strings.Split(reply.Lines()[1], "|")

		ints := make([]int64, len(parts))
		for i, p := range parts {
			ints[i], _ = strconv.ParseInt(p, 10, 32)
		}

		a.TotalEpisodes = int(ints[0])     // episodes
		st := time.Unix(ints[1], 0)        // air date
		et := time.Unix(ints[2], 0)        // end date
		aw := strings.Split(parts[3], "'") // award list
		ut := time.Unix(ints[4], 0)        // update date

		if len(parts[3]) > 0 {
			a.Awards = aw
		}

		// 0 does not actually mean the Epoch here...
		if ints[1] != 0 {
			a.StartDate = st
		}
		if ints[2] != 0 {
			a.EndDate = et
		}
		if ints[4] != 0 {
			a.Updated = ut
		}
		return true
	}
	return false
}
