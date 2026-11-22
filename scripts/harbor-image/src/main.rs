// Matches the PNG/JPEG paths in openai/codex rust-v0.151.0,
// codex-rs/utils/image/src/lib.rs. The recorded attachment fixes the target
// dimensions for this replay; the guest reads, decodes, and encodes its file.
use std::env;
use std::error::Error;
use std::fs;
use std::io::{self, Cursor, Write};

use base64::{engine::general_purpose::STANDARD, Engine};
use image::codecs::jpeg::JpegEncoder;
use image::imageops::FilterType;
use image::{DynamicImage, GenericImageView, ImageDecoder, ImageEncoder, ImageFormat, ImageReader};

fn main() -> Result<(), Box<dyn Error>> {
    let [path, width, height]: [String; 3] = env::args().skip(1).collect::<Vec<_>>()
        .try_into().map_err(|_| "usage: aries-harbor-image PATH WIDTH HEIGHT")?;
    let target = (width.parse::<u32>()?, height.parse::<u32>()?);
    if target.0 == 0 || target.1 == 0 || target.0 > 4096 || target.1 > 4096 {
        return Err("invalid recorded dimensions".into());
    }
    let data = fs::read(path)?;
    let format = image::guess_format(&data)?;
    let mime = match format {
        ImageFormat::Png => "image/png",
        ImageFormat::Jpeg => "image/jpeg",
        _ => return Err("unsupported image format".into()),
    };
    let mut decoder = ImageReader::with_format(Cursor::new(&data), format).into_decoder()?;
    let icc = decoder.icc_profile().ok().flatten()
        .filter(|profile| profile.get(16..20) == Some(b"RGB "));
    let exif = decoder.exif_metadata().ok().flatten();
    let image = DynamicImage::from_decoder(decoder)?;
    let bytes = if image.dimensions() == target {
        data
    } else if format == ImageFormat::Jpeg {
        let resized = image.resize_exact(target.0, target.1, FilterType::Triangle);
        let mut encoded = Vec::new();
        let mut encoder = JpegEncoder::new_with_quality(&mut encoded, 85);
        if let Some(profile) = icc { encoder.set_icc_profile(profile)?; }
        if let Some(exif) = exif { encoder.set_exif_metadata(exif)?; }
        encoder.encode_image(&resized)?;
        encoded
    } else {
        return Err("unexpected PNG resize in reviewed recording".into());
    };
    write!(io::stdout().lock(),
        r#"[{{"type":"input_image","image_url":"data:{mime};base64,{}","detail":"high"}}]"#,
        STANDARD.encode(&bytes))?;
    Ok(())
}
