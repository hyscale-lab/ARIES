package apptainer

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/hyscale-lab/aries/pkg/containerimage"
	"github.com/sirupsen/logrus"
)

// PullImages unpacks each image into a directory under directory, unless one
// is there already: from the source that sources maps it to (a SIF or a
// directory prepared elsewhere), or from its registry. ARIES pins images by
// tag or digest, so an existing directory is the image it names.
//
// Instances run from these unpacked copies rather than from SIF files. An
// unprivileged build makes every file belong to the invoking user, which is
// root inside the instance; in a SIF, files of other users (apt's _apt, for
// one) cannot be changed by that root, and apt-get then fails.
// A build is written beside its destination and renamed into place, so an
// interrupted build never leaves a partial image.
func PullImages(ctx context.Context, apptainerPath, directory string, images []string, sources map[string]string) error {
	if apptainerPath == "" {
		apptainerPath = defaultApptainer
	}
	return pullImages(ctx, execApptainer{path: apptainerPath}, directory, images, sources)
}

func pullImages(ctx context.Context, apptainer commandRunner, directory string, images []string, sources map[string]string) error {
	if directory == "" || !filepath.IsAbs(directory) {
		return errors.New("Apptainer image directory must be absolute")
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create Apptainer image directory: %w", err)
	}
	manager := &Manager{apptainer: apptainer}
	seen := make(map[string]struct{}, len(images))
	for _, image := range images {
		if _, ok := seen[image]; ok {
			continue
		}
		seen[image] = struct{}{}
		source := sources[image]
		if source == "" {
			if err := validatePullImage(image); err != nil {
				return fmt.Errorf("prepare Apptainer image %q: %w", image, err)
			}
			source = "docker://" + image
		} else if !filepath.IsAbs(source) {
			return fmt.Errorf("Apptainer image %q maps to a relative path", image)
		}
		destination := rootfsPath(directory, image)
		if info, err := os.Stat(destination); err == nil {
			if !info.IsDir() {
				return fmt.Errorf("prepared Apptainer image %q is not a directory: %s", image, destination)
			}
			// A setup that stopped after placing the image may not have recorded it.
			if err := recordImage(destination, image); err != nil {
				return fmt.Errorf("record Apptainer image %q: %w", image, err)
			}
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("inspect Apptainer image %q: %w", image, err)
		}
		suffix, err := randomID()
		if err != nil {
			return err
		}
		partial := destination + ".partial-" + suffix
		logrus.WithContext(ctx).WithFields(logrus.Fields{"image": image, "source": source}).Info("unpacking image for Apptainer")
		if _, err := manager.run(ctx, nil, "build", "--disable-cache", "--fix-perms", "--sandbox", partial, source); err != nil {
			removeTree(partial)
			return fmt.Errorf("unpack Apptainer image %q: %w", image, err)
		}
		if err := os.Rename(partial, destination); err != nil {
			removeTree(partial)
			return fmt.Errorf("place Apptainer image %q: %w", image, err)
		}
		if err := recordImage(destination, image); err != nil {
			return fmt.Errorf("record Apptainer image %q: %w", image, err)
		}
	}
	return nil
}

// recordImage keeps the image's reference beside its directory, so a person
// can tell which directory is which. An existing record is left alone.
func recordImage(destination, image string) error {
	if _, err := os.Stat(destination + ".ref"); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return writePrivate(destination+".ref", []byte(image+"\n"))
}

// removeTree deletes a directory that may hold entries without write
// permission, as unpacked images do.
func removeTree(path string) {
	_ = filepath.WalkDir(path, func(entry string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			_ = os.Chmod(entry, 0o700)
		}
		return nil
	})
	_ = os.RemoveAll(path)
}

func validatePullImage(image string) error {
	if err := containerimage.Validate(image); err == nil {
		return nil
	}
	tagged, err := containerimage.ValidateTagOnly(image)
	if err != nil {
		return err
	}
	if tagged != image {
		return errors.New("image must not contain surrounding whitespace")
	}
	return nil
}
