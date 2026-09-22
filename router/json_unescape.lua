-- Decodes JSON string escapes from stdin, writes real UTF-8 bytes to
-- stdout. Used by command_watcher.sh's json_unescape() to undo Telegram's
-- own JSON encoding of a message's text before that text goes anywhere
-- that cares about its real bytes (an SMS body, in particular): the Bot
-- API escapes every non-ASCII character as \uXXXX, so a Cyrillic or emoji
-- reply arrives as literal backslash-u-hex-digit sequences, not the UTF-8
-- bytes a shell sed one-liner could reasonably round-trip.
--
-- Lua 5.1 (this router's stock /usr/bin/lua) has no utf8 library, so
-- codepoint -> UTF-8 is done by hand, and a \uXXXX surrogate pair (used
-- for anything outside the Basic Multilingual Plane, e.g. many emoji) is
-- combined into one codepoint before encoding.
local function utf8enc(cp)
    if cp < 0x80 then
        return string.char(cp)
    elseif cp < 0x800 then
        return string.char(0xC0 + math.floor(cp / 0x40), 0x80 + cp % 0x40)
    elseif cp < 0x10000 then
        return string.char(
            0xE0 + math.floor(cp / 0x1000),
            0x80 + math.floor(cp / 0x40) % 0x40,
            0x80 + cp % 0x40)
    else
        return string.char(
            0xF0 + math.floor(cp / 0x40000),
            0x80 + math.floor(cp / 0x1000) % 0x40,
            0x80 + math.floor(cp / 0x40) % 0x40,
            0x80 + cp % 0x40)
    end
end

local s = io.read("*a") or ""
local out, n, i = {}, #s, 1
while i <= n do
    local c = s:sub(i, i)
    if c == "\\" and i < n then
        local nc = s:sub(i + 1, i + 1)
        if nc == "n" then out[#out + 1], i = "\n", i + 2
        elseif nc == "t" then out[#out + 1], i = "\t", i + 2
        elseif nc == "r" then out[#out + 1], i = "\r", i + 2
        elseif nc == "b" then out[#out + 1], i = "\b", i + 2
        elseif nc == "f" then out[#out + 1], i = "\f", i + 2
        elseif nc == "\"" then out[#out + 1], i = "\"", i + 2
        elseif nc == "\\" then out[#out + 1], i = "\\", i + 2
        elseif nc == "/" then out[#out + 1], i = "/", i + 2
        elseif nc == "u" then
            local cp = tonumber(s:sub(i + 2, i + 5), 16) or 0
            i = i + 6
            if cp >= 0xD800 and cp <= 0xDBFF and s:sub(i, i + 1) == "\\u" then
                local cp2 = tonumber(s:sub(i + 2, i + 5), 16)
                if cp2 and cp2 >= 0xDC00 and cp2 <= 0xDFFF then
                    cp = 0x10000 + (cp - 0xD800) * 0x400 + (cp2 - 0xDC00)
                    i = i + 6
                end
            end
            out[#out + 1] = utf8enc(cp)
        else
            out[#out + 1], i = nc, i + 2
        end
    else
        out[#out + 1], i = c, i + 1
    end
end
io.write(table.concat(out))
