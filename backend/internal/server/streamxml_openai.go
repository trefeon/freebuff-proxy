package server

// OpenAI half of the streaming XML tool-call extraction (issue #151):
// the feed/flush core lives in stream_shared.go (feedXMLToolCalls with the
// streamChatContentToToolCalls bytes wrapper; drainXMLToolCalls); the
// Anthropic half's wrappers live in streamxml_anthropic.go. See
// convert.XMLToolCallExtractor for the incremental parser contract
// (Feed/Flush per stream).
