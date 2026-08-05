//! m77rip-compress — compress a file with m77rip for the A2 CA35 image pipeline.
//!
//! The BootMCU decodes the CA35 payload with `m77rip-decode` (no_std). This host
//! tool is its build-time counterpart: `imgtools a35-header --compressed` wraps
//! the output in the 16-byte boot header with the m77 magic.
//!
//! Usage: m77rip-compress <input> <output>

use std::process::exit;

fn main() {
    let args: Vec<String> = std::env::args().collect();
    if args.len() != 3 {
        eprintln!("usage: m77rip-compress <input> <output>");
        exit(2);
    }
    let (input_path, output_path) = (&args[1], &args[2]);

    let input = std::fs::read(input_path).unwrap_or_else(|e| {
        eprintln!("m77rip-compress: read {input_path}: {e}");
        exit(1);
    });
    let compressed = m77rip::compress(&input);
    std::fs::write(output_path, &compressed).unwrap_or_else(|e| {
        eprintln!("m77rip-compress: write {output_path}: {e}");
        exit(1);
    });
    eprintln!(
        "m77rip-compress: {} -> {} ({} -> {} bytes, {:.1}%)",
        input_path,
        output_path,
        input.len(),
        compressed.len(),
        100.0 * compressed.len() as f64 / input.len().max(1) as f64,
    );
}
