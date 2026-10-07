// JSON encoding of the control messages (docs/HELPER_PROTOCOL.md). This is the
// only translation unit that includes the JSON library.
#pragma once

#include <string>
#include <string_view>

#include "types.hpp"

namespace recon {

// Parses one Go -> helper message. Unknown types and out-of-range values are
// errors (code "bad_message"); unknown fields are ignored for forward compatibility.
Status parseControl(std::string_view json, ControlMsg& out);

std::string encodeCaps(const Caps& caps, int64_t qpcFrequency);
std::string encodeStarted(const Started& s);
std::string encodeStats(const FrameStats& s);
std::string encodeCaptureEvent(const CaptureEvent& e);
// re names the request type that caused the error ("" if none).
std::string encodeError(const Status& s, std::string_view re);

}  // namespace recon
