pub fn whoami() -> &'static str {
    concat!(env!("CARGO_PKG_NAME"), " ", env!("CARGO_PKG_VERSION"))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn whoami_contains_version() {
        assert_eq!(whoami(), "metering-admission 0.1.0");
    }
}