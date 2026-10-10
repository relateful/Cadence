package lavalink

import (
	"strconv"
	"strings"

	"github.com/greed/cadence/internal/player"
	"github.com/greed/cadence/internal/source"
)

type TrackInfo struct {
	Identifier string  `json:"identifier"`
	IsSeekable bool    `json:"isSeekable"`
	Author     string  `json:"author"`
	Length     int64   `json:"length"`
	IsStream   bool    `json:"isStream"`
	Position   int64   `json:"position"`
	Title      string  `json:"title"`
	URI        *string `json:"uri"`
	ArtworkURL *string `json:"artworkUrl"`
	ISRC       *string `json:"isrc"`
	SourceName string  `json:"sourceName"`
}

type Track struct {
	Encoded    string         `json:"encoded"`
	Info       TrackInfo      `json:"info"`
	PluginInfo map[string]any `json:"pluginInfo"`
	UserData   map[string]any `json:"userData"`
}

func optional(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

func FromTrack(t source.Track) Track {
	return Track{
		Encoded: t.Encode(),
		Info: TrackInfo{
			Identifier: t.Identifier,
			IsSeekable: !t.IsStream,
			Author:     t.Author,
			Length:     t.DurationMs,
			IsStream:   t.IsStream,
			Title:      t.Title,
			URI:        optional(t.URI),
			ArtworkURL: optional(t.ArtworkURL),
			ISRC:       optional(t.ISRC),
			SourceName: t.Source,
		},
		PluginInfo: map[string]any{},
		UserData:   map[string]any{},
	}
}

func trackPtr(t *source.Track) *Track {
	if t == nil {
		return nil
	}
	out := FromTrack(*t)
	return &out
}

type Exception struct {
	Message         string `json:"message"`
	Severity        string `json:"severity"`
	Cause           string `json:"cause"`
	CauseStackTrace string `json:"causeStackTrace"`
}

func FromFailure(f *source.Failure) *Exception {
	if f == nil {
		return nil
	}
	return &Exception{Message: f.Message, Severity: string(f.Severity), Cause: f.Cause, CauseStackTrace: ""}
}

func LoadResult(res source.LoadResult) map[string]any {
	tracks := make([]Track, 0, len(res.Tracks))
	for _, t := range res.Tracks {
		tracks = append(tracks, FromTrack(t))
	}
	switch res.Type {
	case source.LoadTrack:
		return map[string]any{"loadType": "track", "data": tracks[0]}
	case source.LoadPlaylist:
		name := ""
		if res.Playlist != nil {
			name = res.Playlist.Name
		}
		return map[string]any{"loadType": "playlist", "data": map[string]any{
			"info":       map[string]any{"name": name, "selectedTrack": -1},
			"pluginInfo": playlistPluginInfo(res.Playlist),
			"tracks":     tracks,
		}}
	case source.LoadSearch:
		return map[string]any{"loadType": "search", "data": tracks}
	case source.LoadError:
		return map[string]any{"loadType": "error", "data": FromFailure(res.Error)}
	}
	return map[string]any{"loadType": "empty", "data": map[string]any{}}
}

func playlistPluginInfo(p *source.Playlist) map[string]any {
	out := map[string]any{}
	if p == nil {
		return out
	}
	if p.ArtworkURL != "" {
		out["artworkUrl"] = p.ArtworkURL
	}
	if p.URI != "" {
		out["url"] = p.URI
	}
	out["totalTracks"] = p.Total
	return out
}

type PlayerState struct {
	Time      int64 `json:"time"`
	Position  int64 `json:"position"`
	Connected bool  `json:"connected"`
	Ping      int64 `json:"ping"`
}

type VoiceState struct {
	Token     string `json:"token"`
	Endpoint  string `json:"endpoint"`
	SessionID string `json:"sessionId"`
	ChannelID string `json:"channelId,omitempty"`
}

type Player struct {
	GuildID string         `json:"guildId"`
	Track   *Track         `json:"track"`
	Volume  int            `json:"volume"`
	Paused  bool           `json:"paused"`
	State   PlayerState    `json:"state"`
	Voice   VoiceState     `json:"voice"`
	Filters player.Filters `json:"filters"`
}

func FromPlayer(guildID string, st player.State, state PlayerState, voice VoiceState) Player {
	return Player{GuildID: guildID, Track: trackPtr(st.Track), Volume: st.Volume, Paused: st.Paused, State: state, Voice: voice, Filters: st.Filters}
}

func PlayerUpdate(guildID string, state PlayerState) map[string]any {
	return map[string]any{"op": "playerUpdate", "guildId": guildID, "state": state}
}

func IsStandardEvent(t player.EventType) bool {
	switch t {
	case player.EventTrackStart, player.EventTrackEnd, player.EventTrackException, player.EventTrackStuck:
		return true
	}
	return false
}

func Event(e player.Event) map[string]any {
	out := map[string]any{"op": "event", "type": string(e.Type), "guildId": e.GuildID}
	if t := trackPtr(e.Track); t != nil {
		out["track"] = t
	}
	switch e.Type {
	case player.EventTrackEnd:
		out["reason"] = string(e.Reason)
	case player.EventTrackException:
		out["exception"] = FromFailure(e.Exception)
	case player.EventTrackStuck:
		out["thresholdMs"] = e.ThresholdMs
	default:
		out["instance"] = e.Instance
		if e.PositionMs != 0 {
			out["positionMs"] = e.PositionMs
		}
		if e.SentAt != 0 {
			out["sentAt"] = e.SentAt
			out["audibleAt"] = e.AudibleAt
		}
		if e.StallMs != 0 {
			out["stallMs"] = e.StallMs
		}
		if e.DurationMs != 0 {
			out["durationMs"] = e.DurationMs
		}
		if into := trackPtr(e.Into); into != nil {
			out["into"] = into
			out["intoPositionMs"] = e.IntoMs
		}
	}
	return out
}

func WebSocketClosed(guildID string, code int, reason string, byRemote bool) map[string]any {
	return map[string]any{"op": "event", "type": "WebSocketClosedEvent", "guildId": guildID, "code": code, "reason": reason, "byRemote": byRemote}
}

type Memory struct {
	Free       uint64 `json:"free"`
	Used       uint64 `json:"used"`
	Allocated  uint64 `json:"allocated"`
	Reservable uint64 `json:"reservable"`
}

type CPU struct {
	Cores        int     `json:"cores"`
	SystemLoad   float64 `json:"systemLoad"`
	LavalinkLoad float64 `json:"lavalinkLoad"`
}

type Stats struct {
	Players        int    `json:"players"`
	PlayingPlayers int    `json:"playingPlayers"`
	Uptime         int64  `json:"uptime"`
	Memory         Memory `json:"memory"`
	CPU            CPU    `json:"cpu"`
}

type Version struct {
	Semver     string `json:"semver"`
	Major      int    `json:"major"`
	Minor      int    `json:"minor"`
	Patch      int    `json:"patch"`
	PreRelease any    `json:"preRelease"`
	Build      any    `json:"build"`
}

func ParseVersion(v string) Version {
	parts := strings.SplitN(v, ".", 3)
	nums := [3]int{}
	for i := 0; i < len(parts) && i < 3; i++ {
		nums[i], _ = strconv.Atoi(parts[i])
	}
	return Version{Semver: v, Major: nums[0], Minor: nums[1], Patch: nums[2]}
}
