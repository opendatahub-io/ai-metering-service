/// escape mirrors Go's escaper: a single pass escaping `\`, `=` and `|` with
/// a backslash, so delimiter characters inside a key or value cannot forge a
/// different resource that serializes to the same sequence.
pub(crate) fn escape(s: &str) -> String {
    let mut out = String::with_capacity(s.len());
    for c in s.chars() {
        match c {
            '\\' => out.push_str("\\\\"),
            '=' => out.push_str("\\="),
            '|' => out.push_str("\\|"),
            _ => out.push(c),
        }
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn escape_escapes_delimiters_in_one_pass() {
        assert_eq!(escape("plain"), "plain");
        assert_eq!(escape(r"a\b"), r"a\\b");
        assert_eq!(escape("b|c=d"), r"b\|c\=d");
        // A backslash introduced by escaping a later character is not
        // itself escaped again — one pass, like Go's strings.NewReplacer.
        assert_eq!(escape(r"\="), r"\\\=");
    }
}
